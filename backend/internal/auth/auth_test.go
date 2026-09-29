package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/ws"
)

// fakeProvider is a minimal OAuth server that enforces PKCE: the token
// endpoint only answers when code_verifier hashes to the challenge the
// authorize request carried.
type fakeProvider struct {
	*httptest.Server
	mu         sync.Mutex
	challenges map[string]string // code -> challenge
	user       string            // JSON returned by /user
}

func newFakeProvider(t *testing.T) *fakeProvider {
	f := &fakeProvider{challenges: map[string]string{}, user: `{"id":42,"login":"octo","name":"Octo Cat","avatar_url":"https://a/42.png"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "PKCE required", http.StatusBadRequest)
			return
		}
		code := "code-" + q.Get("state")
		f.mu.Lock()
		f.challenges[code] = q.Get("code_challenge")
		f.mu.Unlock()
		back, _ := url.Parse(q.Get("redirect_uri"))
		bq := back.Query()
		bq.Set("code", code)
		bq.Set("state", q.Get("state"))
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		challenge, ok := f.challenges[r.Form.Get("code")]
		delete(f.challenges, r.Form.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok","token_type":"bearer"}`)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, f.user)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProvider) provider() *Provider {
	p := GitHub("id", "secret")
	p.OAuth.Endpoint = oauth2.Endpoint{AuthURL: f.URL + "/authorize", TokenURL: f.URL + "/token"}
	p.UserInfoURL = f.URL + "/user"
	return p
}

type app struct {
	*httptest.Server
	svc    *Service
	store  *Memory
	client *http.Client
}

// newApp runs the auth routes behind a real server whose public URL is
// its own address, and a browser-like client with a cookie jar.
func newApp(t *testing.T, cfg Config) *app {
	st := NewMemory()
	a := &app{store: st}
	mux := http.NewServeMux()
	a.Server = httptest.NewServer(mux)
	t.Cleanup(a.Close)
	cfg.PublicURL = a.URL
	a.svc = New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.svc.Register(mux)
	mux.HandleFunc("GET /whoami", func(w http.ResponseWriter, r *http.Request) {
		id, err := a.svc.Authorize(r, r.URL.Query().Get("board"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(id)
	})
	a.client = a.newBrowser()
	return a
}

func (a *app) newBrowser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

// post sends a same-origin POST, decodes a JSON reply into out when it
// is non-nil, and returns the status code.
func (a *app) post(t *testing.T, c *http.Client, path, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, a.URL+path, strings.NewReader(body))
	req.Header.Set("Origin", a.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func get(t *testing.T, c *http.Client, u string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (a *app) whoami(t *testing.T, c *http.Client, board string) (ws.Identity, int) {
	t.Helper()
	resp := get(t, c, a.URL+"/whoami?board="+board)
	defer func() { _ = resp.Body.Close() }()
	var id ws.Identity
	_ = json.NewDecoder(resp.Body).Decode(&id)
	return id, resp.StatusCode
}

func TestOAuthLoginWithPKCE(t *testing.T) {
	fp := newFakeProvider(t)
	a := newApp(t, Config{Providers: []*Provider{fp.provider()}})

	// The client follows every redirect: app -> provider -> callback -> next.
	resp := get(t, a.client, a.URL+"/auth/github/login?next=/%3Froom%3Dr1")
	_ = resp.Body.Close()
	if resp.Request.URL.Path != "/" || resp.Request.URL.Query().Get("room") != "r1" {
		t.Fatalf("ended at %s, want /?room=r1", resp.Request.URL)
	}

	resp = get(t, a.client, a.URL+"/api/auth")
	var status struct {
		Enabled bool
		User    *User
	}
	_ = json.NewDecoder(resp.Body).Decode(&status)
	_ = resp.Body.Close()
	if !status.Enabled || status.User == nil || status.User.Name != "Octo Cat" {
		t.Fatalf("status = %+v", status)
	}

	// The session cookie is HttpOnly and never holds anything the store keeps.
	u, _ := url.Parse(a.URL)
	var session *http.Cookie
	for _, c := range a.client.Jar.Cookies(u) {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie")
	}
	for h := range a.store.sessions {
		if h == session.Value {
			t.Fatal("store holds the raw session token")
		}
	}
}

func TestCallbackRejectsForgedState(t *testing.T) {
	fp := newFakeProvider(t)
	a := newApp(t, Config{Providers: []*Provider{fp.provider()}})
	noRedirect := &http.Client{Jar: a.client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp := get(t, noRedirect, a.URL+"/auth/github/login")
	_ = resp.Body.Close()
	// An attacker's code and state, not the ones this browser started with.
	resp = get(t, noRedirect, a.URL+"/auth/github/callback?code=evil&state=evil")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged callback status = %d, want 400", resp.StatusCode)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/?room=a":           "/?room=a",
		"":                   "/",
		"https://evil.test/": "/",
		"//evil.test":        "/",
		"/\\evil.test":       "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func devLogin(t *testing.T, a *app, c *http.Client, name string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, a.URL+"/auth/dev/login", strings.NewReader(url.Values{"name": {name}, "next": {"/"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", a.URL)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func TestRolesAndInvites(t *testing.T) {
	a := newApp(t, Config{DevLogin: true})
	alice, bob := a.client, a.newBrowser()
	devLogin(t, a, alice, "Alice")
	devLogin(t, a, bob, "Bob")

	// Not signed in: no entry.
	if _, code := a.whoami(t, a.newBrowser(), "b1"); code != http.StatusForbidden {
		t.Fatalf("anonymous = %d, want refusal", code)
	}
	// Alice opens the board first and owns it; Bob is not invited.
	if id, _ := a.whoami(t, alice, "b1"); id.Role != proto.RoleOwner || id.Name != "Alice" {
		t.Fatalf("alice = %+v", id)
	}
	if _, code := a.whoami(t, bob, "b1"); code != http.StatusForbidden {
		t.Fatalf("uninvited bob = %d", code)
	}

	// Only the owner can create invites.
	if code := a.post(t, bob, "/api/boards/b1/invites", `{"role":"editor"}`, nil); code != http.StatusForbidden {
		t.Fatalf("bob inviting = %d", code)
	}
	var inv struct{ Token, URL string }
	if code := a.post(t, alice, "/api/boards/b1/invites", `{"role":"viewer"}`, &inv); code != http.StatusCreated || !strings.Contains(inv.URL, "invite="+inv.Token) {
		t.Fatalf("invite = %d %+v", code, inv)
	}
	if code := a.post(t, bob, "/api/invites/"+inv.Token+"/accept", "", nil); code != http.StatusOK {
		t.Fatalf("accept = %d", code)
	}
	if id, _ := a.whoami(t, bob, "b1"); id.Role != proto.RoleViewer {
		t.Fatalf("bob after invite = %+v", id)
	}
	if code := a.post(t, bob, "/api/invites/nonsense/accept", "", nil); code != http.StatusNotFound {
		t.Fatalf("bogus invite = %d", code)
	}
}

func TestGuestsWatchReadOnly(t *testing.T) {
	a := newApp(t, Config{DevLogin: true, Guests: true})
	id, code := a.whoami(t, a.newBrowser(), "b1")
	if code != http.StatusOK || id.Role != proto.RoleViewer || id.User != "" {
		t.Fatalf("guest = %d %+v", code, id)
	}
}

func TestCrossOriginPostIsRefused(t *testing.T) {
	a := newApp(t, Config{DevLogin: true})
	devLogin(t, a, a.client, "Alice")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, a.URL+"/api/boards/b1/invites", strings.NewReader(`{"role":"editor"}`))
	req.Header.Set("Origin", "https://evil.test")
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin invite = %d", resp.StatusCode)
	}
}

func TestLogoutEndsSession(t *testing.T) {
	a := newApp(t, Config{DevLogin: true})
	devLogin(t, a, a.client, "Alice")
	if _, code := a.whoami(t, a.client, "b1"); code != http.StatusOK {
		t.Fatalf("before logout = %d", code)
	}
	a.post(t, a.client, "/auth/logout", "", nil)
	if _, code := a.whoami(t, a.client, "b1"); code != http.StatusForbidden {
		t.Fatalf("after logout = %d", code)
	}
}

func TestOpenModeWhenNothingIsConfigured(t *testing.T) {
	svc := New(Config{}, NewMemory(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	id, err := svc.Authorize(httptest.NewRequest(http.MethodGet, "/ws", nil).WithContext(context.Background()), "b")
	if err != nil || id.Role != proto.RoleEditor || svc.Enabled() {
		t.Fatalf("open mode = %+v %v", id, err)
	}
}

func TestAccessAnswersBeforeTheSocket(t *testing.T) {
	a := newApp(t, Config{DevLogin: true})
	alice, bob := a.client, a.newBrowser()
	devLogin(t, a, alice, "Alice")
	devLogin(t, a, bob, "Bob")
	access := func(c *http.Client) (int, string) {
		resp := get(t, c, a.URL+"/api/boards/b1/access")
		defer func() { _ = resp.Body.Close() }()
		var body struct{ Role string }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Role
	}
	if code, role := access(alice); code != http.StatusOK || role != string(proto.RoleOwner) {
		t.Fatalf("owner = %d %q", code, role)
	}
	if code, _ := access(bob); code != http.StatusForbidden {
		t.Fatalf("uninvited = %d, want 403", code)
	}
	if code, _ := access(a.newBrowser()); code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", code)
	}
}
