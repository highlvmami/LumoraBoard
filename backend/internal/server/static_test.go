package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatic(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "_app", "immutable"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "index.html"), []byte("app"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "_app", "immutable", "a.js"), []byte("js"), 0o644))
	h := New(Config{ShutdownTimeout: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, NewStatic(dir)).Handler()

	for _, c := range []struct {
		path, body, cache string
		code              int
	}{
		{"/", "app", "no-cache", 200},
		{"/?room=abc", "app", "no-cache", 200},
		{"/board/xyz", "app", "no-cache", 200},
		{"/_app/immutable/a.js", "js", "public, max-age=31536000, immutable", 200},
		{"/etc/passwd", "app", "no-cache", 200},
		{"/healthz", "{\"status\":\"ok\"}\n", "", 200},
		{"/api/nope", "", "", 404},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.code || (c.body != "" && rec.Body.String() != c.body) || rec.Header().Get("Cache-Control") != c.cache {
			t.Errorf("%s: %d %q cache=%q", c.path, rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
		}
	}
}
