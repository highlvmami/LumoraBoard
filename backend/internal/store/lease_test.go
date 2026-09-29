package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

type leaseStore interface {
	Store
	Leases
}

func testLeaseContract(t *testing.T, s leaseStore) {
	t.Helper()
	ctx := context.Background()
	b := fmt.Sprint("lease-", time.Now().UnixNano())
	const ttl = 300 * time.Millisecond

	l, err := s.Acquire(ctx, b, "A", "http://a", ttl)
	if err != nil || l.Instance != "A" || l.Addr != "http://a" {
		t.Fatalf("first acquire = %+v, %v", l, err)
	}
	first := l.Epoch
	if l, _ := s.Acquire(ctx, b, "A", "http://a", ttl); l.Epoch != first {
		t.Fatalf("re-acquire changed epoch %d -> %d", first, l.Epoch)
	}
	// B sees A as the owner while the lease is live.
	if l, _ := s.Acquire(ctx, b, "B", "http://b", ttl); l.Instance != "A" || l.Addr != "http://a" {
		t.Fatalf("B took a live lease: %+v", l)
	}
	if held, err := s.Renew(ctx, "B", []string{b}, ttl); err != nil || len(held) != 0 {
		t.Fatalf("B renewed A's lease: %v %v", held, err)
	}

	// A writes under its fence.
	fenceA := WithFence(ctx, Fence{Instance: "A", Epoch: first})
	rec := []Record{{Seq: 1, From: "c", Op: json.RawMessage(`{"kind":"delete","id":"x"}`)}}
	if err := s.Append(fenceA, b, rec); err != nil {
		t.Fatalf("owner append: %v", err)
	}

	// A lets go (or dies and the lease runs out); B takes over.
	if err := s.Release(ctx, b, "A"); err != nil {
		t.Fatal(err)
	}
	l, err = s.Acquire(ctx, b, "B", "http://b", ttl)
	if err != nil || l.Instance != "B" || l.Epoch <= first {
		t.Fatalf("takeover = %+v, %v", l, err)
	}
	if held, _ := s.Renew(ctx, "A", []string{b}, ttl); len(held) != 0 {
		t.Fatalf("A still renews after takeover: %v", held)
	}

	// Every kind of write from A is now refused; B's go through.
	late := []Record{{Seq: 2, From: "c", Op: json.RawMessage(`{"kind":"delete","id":"y"}`)}}
	for name, err := range map[string]error{
		"append":  s.Append(fenceA, b, late),
		"compact": s.Compact(fenceA, b, Snapshot{Seq: 2}),
		"chat":    s.AppendChat(fenceA, b, []proto.ChatMessage{{ID: 1, Text: "stale", At: time.Now()}}),
	} {
		if !errors.Is(err, ErrFenced) {
			t.Errorf("stale %s = %v, want ErrFenced", name, err)
		}
	}
	fenceB := WithFence(ctx, Fence{Instance: "B", Epoch: l.Epoch})
	if err := s.Append(fenceB, b, late); err != nil {
		t.Fatalf("new owner append: %v", err)
	}
	loaded, _ := s.Load(ctx, b)
	if len(loaded.Ops) != 2 || len(loaded.Chat) != 0 {
		t.Fatalf("loaded %d ops, %d chat", len(loaded.Ops), len(loaded.Chat))
	}

	// Expiry without release also frees the lease.
	time.Sleep(ttl + 100*time.Millisecond)
	if l, _ := s.Acquire(ctx, b, "C", "http://c", ttl); l.Instance != "C" {
		t.Fatalf("expired lease not taken: %+v", l)
	}
	// Renew reports only what is still held.
	other := b + "-2"
	if _, err := s.Acquire(ctx, other, "C", "http://c", ttl); err != nil {
		t.Fatal(err)
	}
	held, err := s.Renew(ctx, "C", []string{b, other, "never-acquired"}, ttl)
	slices.Sort(held)
	if err != nil || !slices.Equal(held, []string{b, other}) {
		t.Fatalf("renew = %v, %v", held, err)
	}
}
