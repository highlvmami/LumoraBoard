package room

import (
	"context"
	"log/slog"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

// persister is a room's write-behind writer. The room hands it records
// through a bounded queue and never waits for the database: the persister
// batches records and writes them on its own goroutine, retrying until
// the store accepts them.
//
// When the database is slow the queue fills up, and the room stops taking
// new ops until there is space again (see Room.run). That pushes the
// back-pressure onto the clients' sockets instead of growing memory or
// dropping accepted ops. Cursor traffic is not persisted and keeps
// flowing meanwhile.
type persister struct {
	room  string
	store store.Store
	cfg   Config
	log   *slog.Logger
	hub   *Hub

	in       chan persistItem // only the room sends, and closes it on exit
	wake     chan struct{}    // signalled after each write, for a stalled room
	shutdown chan struct{}    // closed by the room when the server stops
	done     chan struct{}    // closed when run returns

	giveUpAt time.Time // set once shutdown is noticed
}

// persistItem is exactly one of: a record, a snapshot, or a flush request.
type persistItem struct {
	rec   *store.Record
	snap  *store.Snapshot
	flush chan struct{} // closed once everything queued before it is written
}

func newPersister(room string, hub *Hub) *persister {
	return &persister{
		room:     room,
		store:    hub.cfg.Store,
		cfg:      hub.cfg,
		log:      hub.log.With("room", room, "component", "persister"),
		hub:      hub,
		in:       make(chan persistItem, hub.cfg.PersistQueue),
		wake:     make(chan struct{}, 1),
		shutdown: make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// full reports whether the room must stop accepting ops. One slot stays
// free so a snapshot can always be queued after the op that triggers it.
// Only the room goroutine sends on in, so this cannot go stale between the
// check and the room's next send.
func (p *persister) full() bool { return len(p.in) >= cap(p.in)-1 }

func (p *persister) run() {
	defer close(p.done)

	var batch []store.Record
	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		recs := batch
		p.retry("append", func(ctx context.Context) error { return p.store.Append(ctx, p.room, recs) })
		batch = nil
		p.signal()
	}

	for {
		select {
		case it, ok := <-p.in:
			if !ok {
				flush()
				return
			}
			switch {
			case it.rec != nil:
				batch = append(batch, *it.rec)
				if len(batch) >= p.cfg.BatchSize {
					flush()
				}
			case it.snap != nil:
				// Records before the snapshot go first, so the store
				// never sees a snapshot ahead of ops it has not written.
				flush()
				snap := *it.snap
				p.retry("compact", func(ctx context.Context) error { return p.store.Compact(ctx, p.room, snap) })
				p.signal()
			case it.flush != nil:
				flush()
				close(it.flush)
			}
		case <-ticker.C:
			flush()
		}
	}
}

// signal wakes a room that stopped taking ops because the queue was full.
func (p *persister) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// retry runs fn until it succeeds. While the server runs it never gives
// up: losing an accepted op is worse than stalling the room, and the
// stall is visible (Hub.PersistStalls). Once the server is stopping it
// keeps trying only until the shutdown flush deadline.
func (p *persister) retry(what string, fn func(context.Context) error) {
	backoff := 50 * time.Millisecond
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), p.attemptTimeout())
		err := fn(ctx)
		cancel()
		if err == nil {
			if attempt > 1 {
				p.log.Info("store recovered", "op", what, "attempts", attempt)
			}
			return
		}
		p.hub.persistFailures.Add(1)
		if p.stopping() && time.Now().After(p.giveUpAt) {
			p.log.Error("giving up on store write during shutdown; data since the last write is lost", "op", what, "err", err)
			return
		}
		p.log.Warn("store write failed, retrying", "op", what, "attempt", attempt, "err", err)
		select {
		case <-time.After(backoff):
		case <-p.shutdown:
		}
		backoff = min(backoff*2, 2*time.Second)
	}
}

func (p *persister) stopping() bool {
	select {
	case <-p.shutdown:
		if p.giveUpAt.IsZero() {
			p.giveUpAt = time.Now().Add(p.cfg.ShutdownFlush)
		}
		return true
	default:
		return false
	}
}

func (p *persister) attemptTimeout() time.Duration {
	const perAttempt = 5 * time.Second
	if p.stopping() {
		if left := time.Until(p.giveUpAt); left < perAttempt {
			return max(left, time.Millisecond)
		}
	}
	return perAttempt
}
