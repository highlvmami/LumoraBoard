package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func testServer() *Server {
	return New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

func TestHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Body.String(), "{\"status\":\"ok\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestServeShutsDownOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- testServer().Serve(ctx, ln) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	http.DefaultClient.CloseIdleConnections()
}

func TestVersionAndDebugDump(t *testing.T) {
	s := New(Config{Version: "abc123", DebugToken: "sekret"}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if got := get("/version").Body.String(); got != "{\"version\":\"abc123\"}\n" {
		t.Fatalf("/version = %q", got)
	}
	if rec := get("/debug/goroutines?token=wrong"); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	if rec := get("/debug/goroutines?token=sekret"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "goroutine ") {
		t.Fatalf("dump: %d %q", rec.Code, rec.Body.String())
	}
}

func TestNoDebugDumpWithoutToken(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/goroutines?token=", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
