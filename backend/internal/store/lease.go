package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Lease says which instance runs a board's room.
type Lease struct {
	Board    string
	Instance string
	// Addr is where other instances reach the owner.
	Addr  string
	Epoch uint64
}

// Leases hands out room ownership. Time is the store's own clock, so
// instances with skewed clocks still agree on who holds a lease.
type Leases interface {
	// Acquire makes instance the owner of board if the lease is free,
	// expired or already its own, and returns the current owner either
	// way. Taking over bumps the epoch; renewing one's own keeps it.
	Acquire(ctx context.Context, board, instance, addr string, ttl time.Duration) (Lease, error)
	// Renew extends instance's leases on boards and returns the boards it
	// still holds. A board missing from the result was taken over.
	Renew(ctx context.Context, instance string, boards []string, ttl time.Duration) ([]string, error)
	// Release expires instance's lease on board so another instance can
	// take it at once.
	Release(ctx context.Context, board, instance string) error
}

// Fence identifies the lease a write is made under.
type Fence struct {
	Instance string
	Epoch    uint64
}

type fenceKey struct{}

// WithFence makes writes under ctx succeed only while instance still holds
// the board's lease at that epoch. Without it writes are unconditional,
// which is what a single server wants.
func WithFence(ctx context.Context, f Fence) context.Context {
	return context.WithValue(ctx, fenceKey{}, f)
}

func fenceFrom(ctx context.Context) (Fence, bool) {
	f, ok := ctx.Value(fenceKey{}).(Fence)
	return f, ok
}

// ErrFenced rejects a write from an instance that no longer owns the
// board. Retrying cannot help.
var ErrFenced = errors.New("store: board is owned by another instance")

// ---- memory ------------------------------------------------------------

type memLease struct {
	Lease
	expires time.Time
}

// Acquire implements Leases.
func (m *Memory) Acquire(_ context.Context, board, instance, addr string, ttl time.Duration) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	l, ok := m.leases[board]
	switch {
	case !ok:
		l = &memLease{Lease: Lease{Board: board, Instance: instance, Addr: addr, Epoch: 1}}
		m.leases[board] = l
	case l.Instance == instance:
		l.Addr = addr
	case now.After(l.expires):
		l.Instance, l.Addr = instance, addr
		l.Epoch++
	default:
		return l.Lease, nil
	}
	l.expires = now.Add(ttl)
	return l.Lease, nil
}

// Renew implements Leases.
func (m *Memory) Renew(_ context.Context, instance string, boards []string, ttl time.Duration) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var held []string
	for _, b := range boards {
		if l, ok := m.leases[b]; ok && l.Instance == instance {
			l.expires = time.Now().Add(ttl)
			held = append(held, b)
		}
	}
	return held, nil
}

// Release implements Leases.
func (m *Memory) Release(_ context.Context, board, instance string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.leases[board]; ok && l.Instance == instance {
		l.expires = time.Time{}
	}
	return nil
}

// checkFence must be called with m.mu held.
func (m *Memory) checkFence(ctx context.Context, board string) error {
	f, ok := fenceFrom(ctx)
	if !ok {
		return nil
	}
	if l, held := m.leases[board]; !held || l.Instance != f.Instance || l.Epoch != f.Epoch {
		return ErrFenced
	}
	return nil
}

// ---- postgres ----------------------------------------------------------

// Acquire implements Leases.
func (p *Postgres) Acquire(ctx context.Context, board, instance, addr string, ttl time.Duration) (Lease, error) {
	l := Lease{Board: board}
	var epoch int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO room_leases (board, instance, addr, epoch, expires_at)
		VALUES ($1, $2, $3, 1, now() + $4 * interval '1 millisecond')
		ON CONFLICT (board) DO UPDATE SET
			instance   = excluded.instance,
			addr       = excluded.addr,
			epoch      = CASE WHEN room_leases.instance = excluded.instance
			                  THEN room_leases.epoch ELSE room_leases.epoch + 1 END,
			expires_at = excluded.expires_at
		WHERE room_leases.instance = excluded.instance OR room_leases.expires_at < now()
		RETURNING instance, addr, epoch`,
		board, instance, addr, ttl.Milliseconds()).Scan(&l.Instance, &l.Addr, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		// Someone else holds it.
		err = p.pool.QueryRow(ctx, `SELECT instance, addr, epoch FROM room_leases WHERE board = $1`, board).
			Scan(&l.Instance, &l.Addr, &epoch)
	}
	if err != nil {
		return Lease{}, fmt.Errorf("store: acquire lease %q: %w", board, err)
	}
	l.Epoch = uint64(epoch)
	return l, nil
}

// Renew implements Leases.
func (p *Postgres) Renew(ctx context.Context, instance string, boards []string, ttl time.Duration) ([]string, error) {
	if len(boards) == 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `
		UPDATE room_leases SET expires_at = now() + $3 * interval '1 millisecond'
		WHERE instance = $1 AND board = ANY($2)
		RETURNING board`, instance, boards, ttl.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("store: renew leases: %w", err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: renew leases: %w", err)
	}
	return held, nil
}

// Release implements Leases.
func (p *Postgres) Release(ctx context.Context, board, instance string) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE room_leases SET expires_at = now() - interval '1 second'
		WHERE board = $1 AND instance = $2`, board, instance)
	if err != nil {
		return fmt.Errorf("store: release lease %q: %w", board, err)
	}
	return nil
}

// checkFence runs inside a write's transaction. FOR SHARE holds the lease
// row until commit, so a takeover waits for this write or this write sees
// the takeover; it cannot land in between.
func checkFence(ctx context.Context, tx pgx.Tx, board string) error {
	f, ok := fenceFrom(ctx)
	if !ok {
		return nil
	}
	var one int
	err := tx.QueryRow(ctx, `
		SELECT 1 FROM room_leases WHERE board = $1 AND instance = $2 AND epoch = $3 FOR SHARE`,
		board, f.Instance, int64(f.Epoch)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return fmt.Errorf("store: check lease %q: %w", board, err)
	}
	return nil
}
