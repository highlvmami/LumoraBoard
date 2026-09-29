// Package store is where boards are persisted: the latest snapshot of a
// board plus the ops accepted after it. Rooms never call a Store directly;
// a per-room persister does, off the room goroutine (see package room).
package store

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
)

// Record is one accepted op as the room sequenced it.
type Record struct {
	Seq        uint64
	From       string
	ClientOpID string
	Op         json.RawMessage
}

// Snapshot is the whole board as of Seq.
type Snapshot struct {
	Seq     uint64
	Objects []board.Object
}

// Loaded is what a room starts from: the latest snapshot and the ops
// after it, in seq order.
type Loaded struct {
	Snapshot Snapshot
	Ops      []Record
}

// Store persists boards. Implementations must be safe for concurrent use
// by many rooms, and Append must be idempotent per (board, seq): a retry
// after a write whose result was lost must not fail or duplicate.
type Store interface {
	Load(ctx context.Context, board string) (Loaded, error)
	Append(ctx context.Context, board string, recs []Record) error
	// Compact saves snap as the board's snapshot and deletes the ops it
	// covers. A snapshot older than the stored one is ignored.
	Compact(ctx context.Context, board string, snap Snapshot) error
}

// Nop keeps nothing. It is what the server uses without a database.
type Nop struct{}

func (Nop) Load(context.Context, string) (Loaded, error)    { return Loaded{}, nil }
func (Nop) Append(context.Context, string, []Record) error  { return nil }
func (Nop) Compact(context.Context, string, Snapshot) error { return nil }

// Memory is an in-process Store for tests and local experiments.
type Memory struct {
	mu     sync.Mutex
	boards map[string]*memBoard
}

type memBoard struct {
	snap Snapshot
	ops  map[uint64]Record
}

// NewMemory creates an empty in-memory store.
func NewMemory() *Memory { return &Memory{boards: make(map[string]*memBoard)} }

func (m *Memory) board(name string) *memBoard {
	b, ok := m.boards[name]
	if !ok {
		b = &memBoard{ops: make(map[uint64]Record)}
		m.boards[name] = b
	}
	return b
}

// Load implements Store.
func (m *Memory) Load(_ context.Context, name string) (Loaded, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boards[name]
	if !ok {
		return Loaded{}, nil
	}
	out := Loaded{Snapshot: Snapshot{Seq: b.snap.Seq, Objects: slices.Clone(b.snap.Objects)}}
	for _, r := range b.ops {
		if r.Seq > b.snap.Seq {
			out.Ops = append(out.Ops, r)
		}
	}
	slices.SortFunc(out.Ops, func(a, b Record) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
	return out, nil
}

// Append implements Store.
func (m *Memory) Append(_ context.Context, name string, recs []Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.board(name)
	for _, r := range recs {
		if _, dup := b.ops[r.Seq]; !dup {
			r.Op = slices.Clone(r.Op)
			b.ops[r.Seq] = r
		}
	}
	return nil
}

// Compact implements Store.
func (m *Memory) Compact(_ context.Context, name string, snap Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.board(name)
	if snap.Seq < b.snap.Seq {
		return nil
	}
	b.snap = Snapshot{Seq: snap.Seq, Objects: slices.Clone(snap.Objects)}
	for seq := range b.ops {
		if seq <= snap.Seq {
			delete(b.ops, seq)
		}
	}
	return nil
}

// OpCount reports how many ops are stored for a board, for tests that
// check compaction.
func (m *Memory) OpCount(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.boards[name]; ok {
		return len(b.ops)
	}
	return 0
}
