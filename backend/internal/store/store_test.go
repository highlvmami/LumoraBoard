package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

// testContract runs the same behaviour checks against any Store.
func testContract(t *testing.T, s Store) {
	ctx := context.Background()
	name := fmt.Sprintf("t%d", time.Now().UnixNano())
	rec := func(seq uint64) Record {
		return Record{Seq: seq, From: "a", ClientOpID: fmt.Sprint("c", seq), Op: json.RawMessage(fmt.Sprintf(`{"kind":"delete","id":"x%d"}`, seq))}
	}

	got, err := s.Load(ctx, name)
	if err != nil || got.Snapshot.Seq != 0 || len(got.Ops) != 0 {
		t.Fatalf("empty board = %+v, %v", got, err)
	}

	if err := s.Append(ctx, name, []Record{rec(1), rec(2), rec(3)}); err != nil {
		t.Fatal(err)
	}
	// A retried batch is idempotent.
	if err := s.Append(ctx, name, []Record{rec(3), rec(4)}); err != nil {
		t.Fatalf("retried append: %v", err)
	}
	got, err = s.Load(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Ops) != 4 || got.Ops[0].Seq != 1 || got.Ops[3].Seq != 4 || got.Ops[2].ClientOpID != "c3" || got.Ops[0].From != "a" {
		t.Fatalf("ops = %+v", got.Ops)
	}
	var op board.Op
	if err := json.Unmarshal(got.Ops[1].Op, &op); err != nil || op.ID != "x2" {
		t.Fatalf("op payload %s: %v", got.Ops[1].Op, err)
	}

	snap := Snapshot{Seq: 3, Objects: []board.Object{{ID: "r", Kind: board.KindRect, W: 5, Version: 3, CreatedBy: "a"}}}
	if err := s.Compact(ctx, name, snap); err != nil {
		t.Fatal(err)
	}
	// An older snapshot never replaces a newer one.
	if err := s.Compact(ctx, name, Snapshot{Seq: 2}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Snapshot.Seq != 3 || len(got.Snapshot.Objects) != 1 || got.Snapshot.Objects[0].W != 5 || got.Snapshot.Objects[0].CreatedBy != "a" {
		t.Fatalf("snapshot = %+v", got.Snapshot)
	}
	if len(got.Ops) != 1 || got.Ops[0].Seq != 4 {
		t.Fatalf("ops after compaction = %+v", got.Ops)
	}
}

func testChatContract(t *testing.T, s Store) {
	ctx := context.Background()
	name := fmt.Sprintf("c%d", time.Now().UnixNano())
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	var msgs []proto.ChatMessage
	for i := uint64(1); i <= 7; i++ {
		msgs = append(msgs, proto.ChatMessage{ID: i, From: "c", User: "u", Name: "Ayşe", Text: fmt.Sprint("m", i), At: at})
	}
	if err := s.AppendChat(ctx, name, msgs[:5]); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendChat(ctx, name, msgs[4:]); err != nil { // overlaps: idempotent
		t.Fatal(err)
	}
	got, more, err := s.ChatBefore(ctx, name, 0, 3)
	if err != nil || !more || len(got) != 3 || got[0].ID != 5 || got[2].ID != 7 || got[2].Text != "m7" || !got[2].At.Equal(at) {
		t.Fatalf("latest page = %+v more=%v %v", got, more, err)
	}
	got, more, _ = s.ChatBefore(ctx, name, 5, 10)
	if more || len(got) != 4 || got[0].ID != 1 || got[3].ID != 4 {
		t.Fatalf("older page = %+v more=%v", got, more)
	}
	l, err := s.Load(ctx, name)
	if err != nil || len(l.Chat) != 7 || l.MoreChat {
		t.Fatalf("load chat = %d more=%v %v", len(l.Chat), l.MoreChat, err)
	}
}

func TestMemory(t *testing.T) {
	testContract(t, NewMemory())
	testChatContract(t, NewMemory())
}

// TestPostgres runs against a real database when LUMORA_TEST_DATABASE_URL
// is set (CI sets it); locally it is skipped without one.
func TestPostgres(t *testing.T) {
	url := os.Getenv("LUMORA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LUMORA_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s, err := NewPostgres(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	// Applying the schema twice is fine.
	if _, err := NewPostgres(ctx, pool); err != nil {
		t.Fatalf("second schema apply: %v", err)
	}
	testContract(t, s)
	testChatContract(t, s)
}
