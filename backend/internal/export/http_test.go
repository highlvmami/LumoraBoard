package export

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/store"
	"github.com/highlvmami/lumoraboard/backend/internal/ws"
)

// headerAuth trusts an X-User header: "" is a guest viewer, "nobody" is
// not signed in.
type headerAuth struct{}

func (headerAuth) Authorize(r *http.Request, _ string) (ws.Identity, error) {
	switch u := r.Header.Get("X-User"); u {
	case "nobody":
		return ws.Identity{}, ws.ErrUnauthorized
	case "":
		return ws.Identity{Role: proto.RoleViewer}, nil
	default:
		return ws.Identity{User: u, Role: proto.RoleEditor}, nil
	}
}

type fixture struct {
	srv   *httptest.Server
	store *store.Memory
}

func newFixture(t *testing.T, auth ws.Authorizer) fixture {
	t.Helper()
	st := store.NewMemory()
	m := start(t, Config{}, openSource{}, nil)
	mux := http.NewServeMux()
	var a ws.Authorizer
	if auth != nil {
		a = auth
	}
	NewHandler(m, st, a, "http://app.example", slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fixture{srv: srv, store: st}
}

// reply is what a test needs from a response, read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
}

func (f fixture) do(t *testing.T, method, path, user, origin, body string) (reply, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.Header.Set("X-User", user)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return reply{resp.StatusCode, resp.Header}, data
}

func TestExportOverHTTP(t *testing.T) {
	f := newFixture(t, headerAuth{})
	resp, body := f.do(t, "POST", "/api/boards/demo/exports", "ana", "", `{"format":"pdf"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var st Status
	_ = json.Unmarshal(body, &st)

	// Poll like the client does when a progress message is lost.
	for st.State != StateDone {
		resp, body = f.do(t, "GET", "/api/exports/"+st.ID, "ana", "", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: %d %s", resp.StatusCode, body)
		}
		_ = json.Unmarshal(body, &st)
	}
	resp, body = f.do(t, "GET", st.File, "ana", "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/pdf" ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), `filename="demo.pdf"`) || !strings.HasPrefix(string(body), "%PDF") {
		t.Fatalf("file: %d %v", resp.StatusCode, resp.Header)
	}
	// Someone else cannot see or fetch it.
	for _, path := range []string{"/api/exports/" + st.ID, st.File} {
		if resp, _ := f.do(t, "GET", path, "eve", "", ""); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("eve %s: %d", path, resp.StatusCode)
		}
	}
}

func TestExportRequestErrors(t *testing.T) {
	f := newFixture(t, headerAuth{})
	for _, c := range []struct {
		method, path, user, origin, body string
		want                             int
	}{
		{"POST", "/api/boards/demo/exports", "ana", "", `{"format":"gif"}`, http.StatusBadRequest},
		{"POST", "/api/boards/demo/exports", "ana", "", `nope`, http.StatusBadRequest},
		{"POST", "/api/boards/bad%20name/exports", "ana", "", `{"format":"png"}`, http.StatusBadRequest},
		{"POST", "/api/boards/demo/exports", "nobody", "", `{"format":"png"}`, http.StatusUnauthorized},
		{"POST", "/api/boards/demo/exports", "ana", "https://evil.example", `{"format":"png"}`, http.StatusForbidden},
		{"POST", "/api/boards/demo/exports", "ana", "http://app.example", `{"format":"png"}`, http.StatusAccepted},
		{"GET", "/api/exports/missing", "ana", "", ``, http.StatusNotFound},
		{"DELETE", "/api/exports/missing", "ana", "", ``, http.StatusNotFound},
	} {
		if resp, body := f.do(t, c.method, c.path, c.user, c.origin, c.body); resp.StatusCode != c.want {
			t.Errorf("%s %s as %q from %q: %d %s, want %d", c.method, c.path, c.user, c.origin, resp.StatusCode, body, c.want)
		}
	}
}

func TestImportCreatesABoard(t *testing.T) {
	f := newFixture(t, headerAuth{})
	backup, err := render(context.Background(), FormatJSON, "src", sample(), 1, noProgress)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := f.do(t, "POST", "/api/boards/import", "ana", "", string(backup))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Board   string `json:"board"`
		Objects int    `json:"objects"`
	}
	_ = json.Unmarshal(body, &out)
	if !boardRe.MatchString(out.Board) || !strings.HasPrefix(out.Board, "import-") || out.Objects != len(sample()) {
		t.Fatalf("import result %+v", out)
	}
	loaded, err := f.store.Load(context.Background(), out.Board)
	if err != nil || len(loaded.Snapshot.Objects) != len(sample()) {
		t.Fatalf("stored board: %d objects, %v", len(loaded.Snapshot.Objects), err)
	}

	for name, c := range map[string]struct {
		user, body string
		want       int
	}{
		"guest":      {"", string(backup), http.StatusUnauthorized},
		"signed out": {"nobody", string(backup), http.StatusUnauthorized},
		"garbage":    {"ana", `{"format":"x"}`, http.StatusBadRequest},
		"too big":    {"ana", strings.Repeat(" ", MaxBackupBytes+1), http.StatusRequestEntityTooLarge},
	} {
		if resp, body := f.do(t, "POST", "/api/boards/import", c.user, "", c.body); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", name, resp.StatusCode, body, c.want)
		}
	}
}

func TestOpenModeNeedsNoSignIn(t *testing.T) {
	f := newFixture(t, nil)
	if resp, body := f.do(t, "POST", "/api/boards/demo/exports", "", "", `{"format":"json"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("export: %d %s", resp.StatusCode, body)
	}
	backup, _ := render(context.Background(), FormatJSON, "src", nil, 1, noProgress)
	if resp, body := f.do(t, "POST", "/api/boards/import", "", "", string(backup)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d %s", resp.StatusCode, body)
	}
}
