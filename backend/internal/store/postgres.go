package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

//go:embed schema.sql
var schema string

// Postgres is the production Store.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres applies the schema on pool and returns the store. The
// caller owns the pool.
func NewPostgres(ctx context.Context, pool *pgxpool.Pool) (*Postgres, error) {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// Load implements Store.
func (p *Postgres) Load(ctx context.Context, name string) (Loaded, error) {
	var (
		out     Loaded
		objects []byte
	)
	err := p.pool.QueryRow(ctx, `SELECT seq, objects FROM board_snapshots WHERE board = $1`, name).Scan(&out.Snapshot.Seq, &objects)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Loaded{}, fmt.Errorf("store: load snapshot %q: %w", name, err)
	default:
		if err := json.Unmarshal(objects, &out.Snapshot.Objects); err != nil {
			return Loaded{}, fmt.Errorf("store: decode snapshot %q: %w", name, err)
		}
	}

	rows, err := p.pool.Query(ctx,
		`SELECT seq, sender, client_op_id, op FROM board_ops WHERE board = $1 AND seq > $2 ORDER BY seq`,
		name, out.Snapshot.Seq)
	if err != nil {
		return Loaded{}, fmt.Errorf("store: load ops %q: %w", name, err)
	}
	out.Ops, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Record, error) {
		var (
			r  Record
			op []byte
		)
		err := row.Scan(&r.Seq, &r.From, &r.ClientOpID, &op)
		r.Op = op
		return r, err
	})
	if err != nil {
		return Loaded{}, fmt.Errorf("store: load ops %q: %w", name, err)
	}
	out.Chat, out.MoreChat, err = p.ChatBefore(ctx, name, 0, ChatTail)
	if err != nil {
		return Loaded{}, err
	}
	return out, nil
}

// AppendChat implements Store.
func (p *Postgres) AppendChat(ctx context.Context, name string, msgs []proto.ChatMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, m := range msgs {
		batch.Queue(`
			INSERT INTO chat_messages (board, id, sender, user_id, name, avatar_url, text, ref, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (board, id) DO NOTHING`,
			name, int64(m.ID), m.From, m.User, m.Name, m.Avatar, m.Text, m.Ref, m.At)
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, name); err != nil {
			return err
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("store: append chat %q: %w", name, err)
		}
		return nil
	})
}

// ChatBefore implements Store.
func (p *Postgres) ChatBefore(ctx context.Context, name string, before uint64, limit int) ([]proto.ChatMessage, bool, error) {
	if before == 0 {
		before = math.MaxInt64
	}
	// One extra row tells whether there is more.
	rows, err := p.pool.Query(ctx, `
		SELECT id, sender, user_id, name, avatar_url, text, ref, created_at FROM chat_messages
		WHERE board = $1 AND id < $2 ORDER BY id DESC LIMIT $3`, name, int64(min(before, math.MaxInt64)), limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: chat %q: %w", name, err)
	}
	msgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (proto.ChatMessage, error) {
		var m proto.ChatMessage
		err := row.Scan(&m.ID, &m.From, &m.User, &m.Name, &m.Avatar, &m.Text, &m.Ref, &m.At)
		return m, err
	})
	if err != nil {
		return nil, false, fmt.Errorf("store: chat %q: %w", name, err)
	}
	more := len(msgs) > limit
	if more {
		msgs = msgs[:limit]
	}
	slices.Reverse(msgs)
	return msgs, more, nil
}

// Append implements Store. One statement per batch; ON CONFLICT makes a
// retried batch harmless.
func (p *Postgres) Append(ctx context.Context, name string, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	seqs := make([]int64, len(recs))
	senders := make([]string, len(recs))
	ids := make([]string, len(recs))
	ops := make([]string, len(recs))
	for i, r := range recs {
		seqs[i] = int64(r.Seq)
		senders[i] = r.From
		ids[i] = r.ClientOpID
		ops[i] = string(r.Op)
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, name); err != nil {
			return err
		}
		if err := touchBoard(ctx, tx, name); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO board_ops (board, seq, sender, client_op_id, op)
			SELECT $1, t.seq, t.sender, t.cid, t.op::jsonb
			FROM unnest($2::bigint[], $3::text[], $4::text[], $5::text[]) AS t(seq, sender, cid, op)
			ON CONFLICT (board, seq) DO NOTHING`,
			name, seqs, senders, ids, ops)
		if err != nil {
			return fmt.Errorf("store: append %q: %w", name, err)
		}
		return nil
	})
}

// Compact implements Store.
func (p *Postgres) Compact(ctx context.Context, name string, snap Snapshot) error {
	objects := snap.Objects
	if objects == nil {
		objects = []board.Object{}
	}
	data, err := json.Marshal(objects)
	if err != nil {
		return fmt.Errorf("store: encode snapshot: %w", err)
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, name); err != nil {
			return err
		}
		if err := touchBoard(ctx, tx, name); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO board_snapshots (board, seq, objects) VALUES ($1, $2, $3)
			ON CONFLICT (board) DO UPDATE SET seq = excluded.seq, objects = excluded.objects, created_at = now()
			WHERE board_snapshots.seq <= excluded.seq`,
			name, int64(snap.Seq), data)
		if err != nil {
			return fmt.Errorf("store: save snapshot %q: %w", name, err)
		}
		if tag.RowsAffected() == 0 {
			return nil // a newer snapshot is already stored
		}
		if _, err := tx.Exec(ctx, `DELETE FROM board_ops WHERE board = $1 AND seq <= $2`, name, int64(snap.Seq)); err != nil {
			return fmt.Errorf("store: compact ops %q: %w", name, err)
		}
		return nil
	})
}

func touchBoard(ctx context.Context, tx pgx.Tx, name string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO boards (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET updated_at = now()`, name)
	if err != nil {
		return fmt.Errorf("store: touch board %q: %w", name, err)
	}
	return nil
}
