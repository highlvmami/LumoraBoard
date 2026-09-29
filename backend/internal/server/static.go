package server

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// Static serves the built frontend from dir. Paths that are not files get
// index.html, so the single-page app handles them; API paths never do.
type Static struct{ root fs.FS }

// NewStatic serves the files under dir.
func NewStatic(dir string) Static { return Static{root: os.DirFS(dir)} }

// Register implements Routes.
func (s Static) Register(mux *http.ServeMux) { mux.Handle("GET /", s) }

func (s Static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, p := range []string{"/api/", "/auth/", "/ws"} {
		if strings.HasPrefix(r.URL.Path, p) {
			http.NotFound(w, r)
			return
		}
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if st, err := fs.Stat(s.root, name); err != nil || st.IsDir() {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		name = "index.html"
	}
	switch {
	case strings.HasPrefix(name, "_app/immutable/"):
		// Hashed file names: safe to cache forever.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case name == "index.html":
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, s.root, name)
}
