package room

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

// ErrHubClosed is returned by Join once the hub has stopped.
var ErrHubClosed = errors.New("hub closed")

// Config tunes the hub and every room it creates.
type Config struct {
	// InboundBuffer is how many ops a room queues before Submit blocks.
	InboundBuffer int
	// IdleTimeout is how long an empty room lingers before it is retired.
	IdleTimeout time.Duration
	// OpLogSize is how many recent ops a room keeps for reconnecting
	// clients; a client further behind than that gets a full snapshot.
	OpLogSize int

	// Store persists boards. Nil means store.Nop: nothing survives a
	// restart.
	Store store.Store
	// PersistQueue is how many records a room may have waiting for the
	// store before it stops taking new ops. With the batch being written,
	// at most PersistQueue+BatchSize records per room are held in memory.
	PersistQueue int
	// BatchSize and FlushInterval bound how long a record waits: a batch
	// is written when it is this big or this old, whichever comes first.
	BatchSize     int
	FlushInterval time.Duration
	// SnapshotEvery is how many ops a room accepts between snapshots.
	// Each snapshot lets the store drop the ops it covers.
	SnapshotEvery int
	// ShutdownFlush is how long a stopping server keeps retrying writes.
	ShutdownFlush time.Duration

	// ChatBurst and ChatRate limit chat per account (per connection for
	// anonymous members): bursts of ChatBurst, refilled at ChatRate/s.
	ChatBurst int
	ChatRate  float64
}

// withDefaults fills persistence settings a caller left zero.
func (c Config) withDefaults() Config {
	if c.Store == nil {
		c.Store = store.Nop{}
	}
	if c.PersistQueue < 2 {
		c.PersistQueue = 1024
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 256
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 100 * time.Millisecond
	}
	if c.SnapshotEvery <= 0 {
		c.SnapshotEvery = 1000
	}
	if c.ShutdownFlush <= 0 {
		c.ShutdownFlush = 5 * time.Second
	}
	if c.ChatBurst <= 0 {
		c.ChatBurst = 5
	}
	if c.ChatRate <= 0 {
		c.ChatRate = 1
	}
	return c
}

// DefaultConfig is what the server uses unless told otherwise.
func DefaultConfig() Config {
	return Config{InboundBuffer: 256, IdleTimeout: time.Minute, OpLogSize: 2000}.withDefaults()
}

// Hub owns the room table. Like a room it is a single goroutine: joins and
// retirements are serialized through it, which is what makes the room
// lifecycle race free without a lock.
type Hub struct {
	cfg Config
	log *slog.Logger

	joinCh   chan hubJoin
	retireCh chan retireReq
	countCh  chan chan int
	closed   chan struct{}
	wg       sync.WaitGroup

	rooms           map[string]*Room // owned by Run
	slowDrops       atomic.Int64
	cursorDrops     atomic.Int64
	persistStalls   atomic.Int64
	persistFailures atomic.Int64
}

type hubJoin struct {
	room   string
	client *Client
	since  uint64
	reply  chan hubJoinResult
}

type hubJoinResult struct {
	room *Room
	err  error
}

type retireReq struct {
	room  *Room
	reply chan bool
}

// NewHub creates a hub. Call Run before Join.
func NewHub(cfg Config, log *slog.Logger) *Hub {
	return &Hub{
		cfg:      cfg.withDefaults(),
		log:      log,
		joinCh:   make(chan hubJoin),
		retireCh: make(chan retireReq),
		countCh:  make(chan chan int),
		closed:   make(chan struct{}),
		rooms:    make(map[string]*Room),
	}
}

// Run drives the hub until ctx is cancelled, then waits for every room to
// close its clients and exit. It always returns nil; the signature matches
// errgroup for the caller's convenience.
func (h *Hub) Run(ctx context.Context) error {
	defer h.wg.Wait()
	defer close(h.closed)

	for {
		select {
		case <-ctx.Done():
			return nil

		case req := <-h.joinCh:
			rm, ok := h.rooms[req.room]
			if !ok {
				rm = newRoom(req.room, h)
				h.rooms[req.room] = rm
				h.wg.Add(1)
				go rm.run(ctx)
			}
			// Forward to the room, which answers the caller directly. The
			// hub does not wait for that answer: a room still loading its
			// board from the store must not hold up joins to other rooms.
			// joinCh is buffered and the room drains it promptly, loading
			// or not, so this send does not block for long.
			rm.joinCh <- joinReq{client: req.client, since: req.since, reply: req.reply}

		case req := <-h.retireCh:
			// Approve only if no join is already queued for that room.
			ok := h.rooms[req.room.name] == req.room && len(req.room.joinCh) == 0
			if ok {
				delete(h.rooms, req.room.name)
				h.log.Debug("retired idle room", "room", req.room.name)
			}
			req.reply <- ok

		case reply := <-h.countCh:
			reply <- len(h.rooms)
		}
	}
}

// Join adds c to the named room, creating the room if needed, and returns
// the room handle the client uses for Submit and Leave. since is the last
// seq the client saw on a previous connection, or 0 for a fresh join.
func (h *Hub) Join(ctx context.Context, room string, c *Client, since uint64) (*Room, error) {
	req := hubJoin{room: room, client: c, since: since, reply: make(chan hubJoinResult, 1)}
	select {
	case h.joinCh <- req:
	case <-h.closed:
		return nil, ErrHubClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	res := <-req.reply
	return res.room, res.err
}

// SlowDrops reports how many clients rooms have dropped for not keeping up.
func (h *Hub) SlowDrops() int64 { return h.slowDrops.Load() }

// PersistStalls reports how many times a room paused taking ops because
// its store writes fell behind.
func (h *Hub) PersistStalls() int64 { return h.persistStalls.Load() }

// PersistFailures reports how many store writes failed (and were retried).
func (h *Hub) PersistFailures() int64 { return h.persistFailures.Load() }

// CursorDrops reports how many presence updates were discarded because a
// room or a recipient was busy. Dropping them is by design.
func (h *Hub) CursorDrops() int64 { return h.cursorDrops.Load() }

// RoomCount reports how many rooms exist. It goes through the hub goroutine,
// so it is exact but not free; use it for tests and diagnostics.
func (h *Hub) RoomCount(ctx context.Context) (int, error) {
	reply := make(chan int, 1)
	select {
	case h.countCh <- reply:
	case <-h.closed:
		return 0, ErrHubClosed
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return <-reply, nil
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("room: marshal payload: " + err.Error())
	}
	return data
}
