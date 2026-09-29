// Package store is where boards are persisted: the latest snapshot of a
// board plus the ops accepted after it. Rooms never call a Store directly;
// a per-room persister does, off the room goroutine (see package room).
package store

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
	"github.com/highlvmami/lumoraboard/backend/internal/proto"
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
	// Chat is the newest chat messages, oldest first, at most ChatTail.
	Chat []proto.ChatMessage
	// MoreChat says whether older messages exist than those in Chat.
	MoreChat bool
}

// ChatTail is how many recent chat messages a room loads and keeps.
const ChatTail = 50

// Store persists boards. Implementations must be safe for concurrent use
// by many rooms, and Append must be idempotent per (board, seq): a retry
// after a write whose result was lost must not fail or duplicate.
type Store interface {
	Load(ctx context.Context, board string) (Loaded, error)
	Append(ctx context.Context, board string, recs []Record) error
	// Compact saves snap as the board's snapshot and deletes the ops it
	// covers. A snapshot older than the stored one is ignored.
	Compact(ctx context.Context, board string, snap Snapshot) error
	// AppendChat stores sequenced chat messages; idempotent per id.
	AppendChat(ctx context.Context, board string, msgs []proto.ChatMessage) error
	// ChatBefore returns up to limit messages with id < before (0: the
	// newest), oldest first, and whether older ones remain.
	ChatBefore(ctx context.Context, board string, before uint64, limit int) ([]proto.ChatMessage, bool, error)
}

// Nop keeps nothing. It is what the server uses without a database.
type Nop struct{}

func (Nop) Load(context.Context, string) (Loaded, error)                  { return Loaded{}, nil }
func (Nop) Append(context.Context, string, []Record) error                { return nil }
func (Nop) Compact(context.Context, string, Snapshot) error               { return nil }
func (Nop) AppendChat(context.Context, string, []proto.ChatMessage) error { return nil }
func (Nop) ChatBefore(context.Context, string, uint64, int) ([]proto.ChatMessage, bool, error) {
	return nil, false, nil
}

// Memory is an in-process Store for tests and local experiments.
type Memory struct {
	mu     sync.Mutex
	boards map[string]*memBoard
}

type memBoard struct {
	snap Snapshot
	ops  map[uint64]Record
	chat []proto.ChatMessage // ascending by id
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
	out.Chat, out.MoreChat = b.chatBefore(0, ChatTail)
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

// AppendChat implements Store.
func (m *Memory) AppendChat(_ context.Context, name string, msgs []proto.ChatMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.board(name)
	for _, msg := range msgs {
		i, found := slices.BinarySearchFunc(b.chat, msg.ID, func(c proto.ChatMessage, id uint64) int { return cmp.Compare(c.ID, id) })
		if !found {
			b.chat = slices.Insert(b.chat, i, msg)
		}
	}
	return nil
}

// ChatBefore implements Store.
func (m *Memory) ChatBefore(_ context.Context, name string, before uint64, limit int) ([]proto.ChatMessage, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boards[name]
	if !ok {
		return nil, false, nil
	}
	msgs, more := b.chatBefore(before, limit)
	return msgs, more, nil
}

func (b *memBoard) chatBefore(before uint64, limit int) ([]proto.ChatMessage, bool) {
	end := len(b.chat)
	if before > 0 {
		end, _ = slices.BinarySearchFunc(b.chat, before, func(c proto.ChatMessage, id uint64) int { return cmp.Compare(c.ID, id) })
	}
	start := max(0, end-limit)
	return slices.Clone(b.chat[start:end]), start > 0
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
