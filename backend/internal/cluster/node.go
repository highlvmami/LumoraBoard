// Package cluster lets several servers share boards. Every board's room
// runs on exactly one instance, the holder of the board's lease in the
// store; other instances forward their clients to it (see package ws).
// When an instance dies its leases run out and the next join elsewhere
// takes the board over from the store.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

// Config tunes leasing.
type Config struct {
	// Instance names this server; random when empty. It must differ
	// between servers and between restarts of one server.
	Instance string
	// Addr is the base URL other instances use to reach this one, such
	// as http://10.0.0.5:8080.
	Addr string
	// TTL is how long a lease lasts without renewal: an upper bound on
	// how long a board stays unreachable after its owner dies.
	TTL time.Duration
}

func (c Config) withDefaults() Config {
	if c.Instance == "" {
		var b [6]byte
		_, _ = rand.Read(b[:])
		c.Instance = hex.EncodeToString(b[:])
	}
	if c.TTL <= 0 {
		c.TTL = 6 * time.Second
	}
	return c
}

// Rooms is what the node needs from the local hub.
type Rooms interface {
	Rooms(ctx context.Context) ([]string, error)
	Evict(ctx context.Context, board string)
}

// held is a lease this instance holds, as far as it knows.
type held struct {
	fence store.Fence
	// until is when this instance stops trusting the lease without a
	// renewal: a margin short of the store's expiry, measured from when
	// the acquire or renew started, so a slow reply cannot stretch it.
	until time.Time
	// since is when it was acquired; a fresh lease is kept even before
	// its room exists.
	since time.Time
}

// Node tracks this instance's leases. It answers the hub's Claim from
// memory and keeps the leases alive in the background.
type Node struct {
	cfg    Config
	leases store.Leases
	log    *slog.Logger
	rooms  Rooms

	mu   sync.Mutex
	held map[string]*held
}

// New creates a node. Call Attach with the hub, then Run.
func New(cfg Config, leases store.Leases, log *slog.Logger) *Node {
	cfg = cfg.withDefaults()
	return &Node{cfg: cfg, leases: leases, log: log.With("component", "cluster", "instance", cfg.Instance), held: make(map[string]*held)}
}

// Attach connects the node to the hub whose rooms it owns. The hub and
// the node refer to each other, so one of them is set after creation.
func (n *Node) Attach(r Rooms) { n.rooms = r }

// Instance returns this node's name.
func (n *Node) Instance() string { return n.cfg.Instance }

// margin is how far before the store's expiry this node stops trusting a
// lease. The gap absorbs clock drift and a renewal that is slow to land.
func (n *Node) margin() time.Duration { return n.cfg.TTL / 4 }

// Owner returns who runs board, taking the lease when it is free. self is
// true when that is this instance.
func (n *Node) Owner(ctx context.Context, board string) (lease store.Lease, self bool, err error) {
	now := time.Now()
	n.mu.Lock()
	if h, ok := n.held[board]; ok && now.Before(h.until) {
		n.mu.Unlock()
		return store.Lease{Board: board, Instance: n.cfg.Instance, Addr: n.cfg.Addr, Epoch: h.fence.Epoch}, true, nil
	}
	n.mu.Unlock()

	l, err := n.leases.Acquire(ctx, board, n.cfg.Instance, n.cfg.Addr, n.cfg.TTL)
	if err != nil {
		return store.Lease{}, false, err
	}
	if l.Instance != n.cfg.Instance {
		return l, false, nil
	}
	n.mu.Lock()
	n.held[board] = &held{
		fence: store.Fence{Instance: n.cfg.Instance, Epoch: l.Epoch},
		until: now.Add(n.cfg.TTL - n.margin()),
		since: now,
	}
	n.mu.Unlock()
	return l, true, nil
}

// Claim implements room.Ownership.
func (n *Node) Claim(board string) (store.Fence, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	h, ok := n.held[board]
	if !ok || !time.Now().Before(h.until) {
		return store.Fence{}, false
	}
	return h.fence, true
}

// Run renews leases until ctx ends. It does not release them: the rooms
// may still be flushing, and a new owner must not load the board before
// that is done. Call Release once the hub has stopped.
func (n *Node) Run(ctx context.Context) error {
	if n.rooms == nil {
		return errors.New("cluster: Run before Attach")
	}
	tick := time.NewTicker(n.cfg.TTL / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			n.renew(ctx)
		}
	}
}

func (n *Node) renew(ctx context.Context) {
	started := time.Now()
	live, err := n.rooms.Rooms(ctx)
	if err != nil {
		return
	}
	liveSet := make(map[string]bool, len(live))
	for _, b := range live {
		liveSet[b] = true
	}

	// Keep leases of live rooms and of ones just taken (their room may
	// be starting); let the rest go.
	n.mu.Lock()
	var keep, drop []string
	for b, h := range n.held {
		if liveSet[b] || started.Sub(h.since) < n.cfg.TTL {
			keep = append(keep, b)
		} else {
			drop = append(drop, b)
			delete(n.held, b)
		}
	}
	n.mu.Unlock()

	for _, b := range drop {
		if err := n.leases.Release(ctx, b, n.cfg.Instance); err != nil {
			n.log.Warn("release failed; the lease will expire", "board", b, "err", err)
		}
	}

	rctx, cancel := context.WithTimeout(ctx, n.cfg.TTL/3)
	renewed, err := n.leases.Renew(rctx, n.cfg.Instance, keep, n.cfg.TTL)
	cancel()
	if err != nil {
		n.log.Warn("lease renewal failed", "err", err)
		n.fenceExpired(ctx)
		return
	}
	ok := make(map[string]bool, len(renewed))
	for _, b := range renewed {
		ok[b] = true
	}
	var lost []string
	n.mu.Lock()
	for _, b := range keep {
		h, still := n.held[b]
		if !still {
			continue
		}
		if ok[b] {
			h.until = started.Add(n.cfg.TTL - n.margin())
		} else {
			delete(n.held, b)
			lost = append(lost, b)
		}
	}
	n.mu.Unlock()
	for _, b := range lost {
		n.log.Warn("lease taken over", "board", b)
		n.rooms.Evict(ctx, b)
	}
}

// fenceExpired closes rooms whose lease this node can no longer vouch
// for. Without the store it cannot renew, and another instance may take
// the board once the lease runs out; serving on would split the room.
func (n *Node) fenceExpired(ctx context.Context) {
	now := time.Now()
	var gone []string
	n.mu.Lock()
	for b, h := range n.held {
		if !now.Before(h.until) {
			delete(n.held, b)
			gone = append(gone, b)
		}
	}
	n.mu.Unlock()
	for _, b := range gone {
		n.log.Error("could not renew lease in time; closing room", "board", b)
		n.rooms.Evict(ctx, b)
	}
}

// Release gives up every lease so other instances can take the boards
// over at once instead of waiting for expiry. Call it after the hub has
// stopped and flushed its rooms.
func (n *Node) Release() {
	n.mu.Lock()
	boards := make([]string, 0, len(n.held))
	for b := range n.held {
		boards = append(boards, b)
	}
	n.held = make(map[string]*held)
	n.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, b := range boards {
		if err := n.leases.Release(ctx, b, n.cfg.Instance); err != nil {
			n.log.Warn("release on shutdown failed; the lease will expire", "board", b, "err", err)
		}
	}
}
