package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

func testStoreContract(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now()
	tag := fmt.Sprint(time.Now().UnixNano())
	board := "b" + tag

	alice, err := s.UpsertUser(ctx, "github", Profile{Subject: "a" + tag, Name: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.UpsertUser(ctx, "github", Profile{Subject: "a" + tag, Name: "Alice B", Avatar: "x.png"})
	if err != nil || again.ID != alice.ID || again.Name != "Alice B" {
		t.Fatalf("second sign-in = %+v, %v; want same user, new name", again, err)
	}
	other, _ := s.UpsertUser(ctx, "google", Profile{Subject: "a" + tag, Name: "Not Alice"})
	if other.ID == alice.ID {
		t.Fatal("same subject at another provider became the same user")
	}
	bob, _ := s.UpsertUser(ctx, "github", Profile{Subject: "b" + tag, Name: "Bob"})

	// Sessions.
	_, hash := newToken()
	if err := s.CreateSession(ctx, hash, alice.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if u, err := s.SessionUser(ctx, hash, now); err != nil || u.ID != alice.ID || u.Avatar != "x.png" {
		t.Fatalf("session user = %+v, %v", u, err)
	}
	if _, err := s.SessionUser(ctx, hash, now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: %v", err)
	}
	if err := s.DeleteSession(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(ctx, hash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session: %v", err)
	}

	// First opener owns the board; others have no role until invited.
	if r, err := s.ClaimBoard(ctx, board, alice.ID); err != nil || r != proto.RoleOwner {
		t.Fatalf("alice claim = %q, %v", r, err)
	}
	if r, _ := s.ClaimBoard(ctx, board, bob.ID); r != "" {
		t.Fatalf("bob role before invite = %q", r)
	}
	if r, _ := s.ClaimBoard(ctx, board, alice.ID); r != proto.RoleOwner {
		t.Fatalf("alice second claim = %q", r)
	}

	// Invites grant a role and never lower one.
	_, edit := newToken()
	_, view := newToken()
	_, stale := newToken()
	must(t, s.CreateInvite(ctx, edit, board, proto.RoleEditor, alice.ID, now.Add(time.Hour)))
	must(t, s.CreateInvite(ctx, view, board, proto.RoleViewer, alice.ID, now.Add(time.Hour)))
	must(t, s.CreateInvite(ctx, stale, board, proto.RoleEditor, alice.ID, now.Add(-time.Minute)))

	if b, r, err := s.AcceptInvite(ctx, view, bob.ID, now); err != nil || b != board || r != proto.RoleViewer {
		t.Fatalf("accept viewer = %q %q %v", b, r, err)
	}
	if _, r, _ := s.AcceptInvite(ctx, edit, bob.ID, now); r != proto.RoleEditor {
		t.Fatalf("upgrade to editor = %q", r)
	}
	if _, r, _ := s.AcceptInvite(ctx, view, bob.ID, now); r != proto.RoleEditor {
		t.Fatalf("viewer invite lowered editor to %q", r)
	}
	if r, _ := s.ClaimBoard(ctx, board, bob.ID); r != proto.RoleEditor {
		t.Fatalf("bob role after invites = %q", r)
	}
	if _, r, _ := s.AcceptInvite(ctx, view, alice.ID, now); r != proto.RoleOwner {
		t.Fatalf("owner accepting an invite became %q", r)
	}
	if _, _, err := s.AcceptInvite(ctx, stale, bob.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired invite: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestMemoryStore(t *testing.T) { testStoreContract(t, NewMemory()) }

func TestPostgresStore(t *testing.T) {
	url := os.Getenv("LUMORA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LUMORA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s, err := NewPostgres(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	testStoreContract(t, s)
}

// Many users opening a fresh board at once: exactly one becomes owner.
func TestPostgresClaimIsAtomic(t *testing.T) {
	url := os.Getenv("LUMORA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LUMORA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s, err := NewPostgres(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	board := fmt.Sprint("race", time.Now().UnixNano())
	const n = 20
	users := make([]User, n)
	for i := range users {
		users[i], _ = s.UpsertUser(ctx, "dev", Profile{Subject: fmt.Sprint(board, i), Name: "u"})
	}
	roles := make(chan proto.Role, n)
	for _, u := range users {
		go func() {
			r, err := s.ClaimBoard(ctx, board, u.ID)
			if err != nil {
				t.Error(err)
			}
			roles <- r
		}()
	}
	owners := 0
	for range n {
		if <-roles == proto.RoleOwner {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("%d owners, want 1", owners)
	}
}
