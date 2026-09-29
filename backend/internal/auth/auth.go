// Package auth is sign-in and board access: OAuth 2 (authorization code
// with PKCE) against GitHub and Google, server-side sessions in an
// HttpOnly cookie, and per-board roles granted by ownership or invites.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/oauth2"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/ws"
)

const (
	sessionCookie = "lumora_session"
	flowCookie    = "lumora_oauth"
	devProviderID = "dev"
)

// Config tunes the Service.
type Config struct {
	// PublicURL is where browsers reach the app, e.g. https://board.example
	// or http://localhost:5173 in development. Callback URLs are built
	// from it, and cookies are Secure when it is https.
	PublicURL string
	Providers []*Provider
	// DevLogin adds a provider that signs in with just a name. For local
	// development and browser tests only; never enable it in production.
	DevLogin bool
	// Guests lets people who are not signed in, or not invited, watch a
	// board read-only.
	Guests     bool
	SessionTTL time.Duration
	InviteTTL  time.Duration
	// HTTPClient talks to providers; nil means http.DefaultClient.
	HTTPClient *http.Client
	Now        func() time.Time
}

// Service implements the auth routes and ws.Authorizer.
type Service struct {
	cfg    Config
	store  Store
	log    *slog.Logger
	secure bool
	byID   map[string]*Provider
}

// New builds the service.
func New(cfg Config, store Store, log *slog.Logger) *Service {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.InviteTTL <= 0 {
		cfg.InviteTTL = 7 * 24 * time.Hour
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	s := &Service{cfg: cfg, store: store, log: log, secure: strings.HasPrefix(cfg.PublicURL, "https://"), byID: map[string]*Provider{}}
	for _, p := range cfg.Providers {
		p.OAuth.RedirectURL = cfg.PublicURL + "/auth/" + p.ID + "/callback"
		s.byID[p.ID] = p
	}
	return s
}

// Enabled reports whether any way to sign in is configured. Without one
// the server runs open: everyone may draw, as before sign-in existed.
func (s *Service) Enabled() bool { return len(s.cfg.Providers) > 0 || s.cfg.DevLogin }

// Register adds the auth routes to mux.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth", s.handleStatus)
	mux.HandleFunc("GET /auth/{provider}/login", s.handleLogin)
	mux.HandleFunc("GET /auth/{provider}/callback", s.handleCallback)
	mux.HandleFunc("POST /auth/dev/login", s.sameOrigin(s.handleDevLogin))
	mux.HandleFunc("POST /auth/logout", s.sameOrigin(s.handleLogout))
	mux.HandleFunc("POST /api/boards/{board}/invites", s.sameOrigin(s.handleCreateInvite))
	mux.HandleFunc("POST /api/invites/{token}/accept", s.sameOrigin(s.handleAcceptInvite))
	mux.HandleFunc("GET /api/boards/{board}/access", s.handleAccess)
}

// handleAccess answers, before any socket is opened, whether the caller
// may open a board: 200 with the role, 401 to sign in, 403 for no access.
// A socket can only say so with a close code, which some proxies lose
// when it follows the upgrade at once; an HTTP status always arrives.
func (s *Service) handleAccess(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	id, err := s.Authorize(r.WithContext(ctx), r.PathValue("board"))
	switch {
	case errors.Is(err, ws.ErrUnauthorized):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, ws.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case err != nil:
		s.fail(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"role": id.Role})
	}
}

// ---- ws.Authorizer -------------------------------------------------------

// Authorize decides who a WebSocket upgrade is and what it may do on board.
func (s *Service) Authorize(r *http.Request, board string) (ws.Identity, error) {
	if !s.Enabled() {
		return ws.Identity{Role: proto.RoleEditor}, nil
	}
	u, err := s.currentUser(r)
	switch {
	case errors.Is(err, ErrNotFound):
		if s.cfg.Guests {
			return ws.Identity{Role: proto.RoleViewer}, nil
		}
		return ws.Identity{}, ws.ErrUnauthorized
	case err != nil:
		return ws.Identity{}, err
	}
	role, err := s.store.ClaimBoard(r.Context(), board, u.ID)
	if err != nil {
		return ws.Identity{}, err
	}
	if role == "" {
		if !s.cfg.Guests {
			return ws.Identity{}, ws.ErrForbidden
		}
		role = proto.RoleViewer
	}
	return ws.Identity{User: u.ID, Name: u.Name, Avatar: u.Avatar, Role: role}, nil
}

func (s *Service) currentUser(r *http.Request) (User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return User{}, ErrNotFound
	}
	return s.store.SessionUser(r.Context(), hashToken(c.Value), s.cfg.Now())
}

// ---- status --------------------------------------------------------------

type providerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Enabled   bool           `json:"enabled"`
		Guests    bool           `json:"guests"`
		Providers []providerInfo `json:"providers"`
		User      *User          `json:"user,omitempty"`
	}{Enabled: s.Enabled(), Guests: s.cfg.Guests, Providers: []providerInfo{}}
	for _, p := range s.cfg.Providers {
		out.Providers = append(out.Providers, providerInfo{p.ID, p.Name})
	}
	if s.cfg.DevLogin {
		out.Providers = append(out.Providers, providerInfo{devProviderID, "Dev login"})
	}
	if u, err := s.currentUser(r); err == nil {
		out.User = &u
	} else if !errors.Is(err, ErrNotFound) {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- OAuth flow ------------------------------------------------------------

// flow is what the browser carries between login and callback, in a
// short-lived HttpOnly cookie scoped to /auth/.
type flow struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Next     string `json:"n"`
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	id := r.PathValue("provider")
	if id == devProviderID && s.cfg.DevLogin {
		s.devForm(w, next)
		return
	}
	p, ok := s.byID[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	state, _ := newToken()
	f := flow{State: state, Verifier: oauth2.GenerateVerifier(), Next: next}
	data, _ := json.Marshal(f)
	http.SetCookie(w, &http.Cookie{
		Name:     flowCookie,
		Value:    base64.RawURLEncoding.EncodeToString(data),
		Path:     "/auth/",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   s.secure,
		// Lax, not Strict: the provider sends the browser back with a
		// top-level GET, and the cookie must come along.
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, p.OAuth.AuthCodeURL(state, oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

func (s *Service) handleCallback(w http.ResponseWriter, r *http.Request) {
	p, ok := s.byID[r.PathValue("provider")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := readFlow(r)
	// The flow cookie is single use.
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: s.secure})
	if err != nil || f.State == "" || r.URL.Query().Get("state") != f.State {
		http.Error(w, "sign-in expired or was started elsewhere; please try again", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Redirect(w, r, f.Next, http.StatusFound) // the person cancelled
		return
	}

	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), oauth2.HTTPClient, s.cfg.HTTPClient), 15*time.Second)
	defer cancel()
	tok, err := p.OAuth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		s.log.Warn("oauth exchange failed", "provider", p.ID, "err", err)
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	prof, err := p.fetchProfile(ctx, s.cfg.HTTPClient, tok)
	if err != nil {
		s.log.Warn("oauth profile failed", "provider", p.ID, "err", err)
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	if err := s.signIn(w, r, p.ID, prof); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, f.Next, http.StatusFound)
}

func readFlow(r *http.Request) (flow, error) {
	c, err := r.Cookie(flowCookie)
	if err != nil {
		return flow{}, err
	}
	data, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return flow{}, err
	}
	var f flow
	err = json.Unmarshal(data, &f)
	f.Next = safeNext(f.Next)
	return f, err
}

func (s *Service) signIn(w http.ResponseWriter, r *http.Request, provider string, prof Profile) error {
	u, err := s.store.UpsertUser(r.Context(), provider, prof)
	if err != nil {
		return err
	}
	token, hash := newToken()
	if err := s.store.CreateSession(r.Context(), hash, u.ID, s.cfg.Now().Add(s.cfg.SessionTTL)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.cfg.SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	s.log.Info("signed in", "provider", provider, "user", u.ID)
	return nil
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.store.DeleteSession(r.Context(), hashToken(c.Value)); err != nil {
			s.fail(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

// ---- dev login ---------------------------------------------------------------

var devFormTmpl = template.Must(template.New("dev").Parse(`<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Dev login · LumoraBoard</title>
<style>body{font:16px system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;margin:0;background:#fafafa}
form{display:grid;gap:.75rem;padding:1.5rem;background:#fff;border:1px solid #e4e4e7;border-radius:12px;width:min(20rem,90vw)}
input,button{font:inherit;padding:.5rem;border-radius:6px;border:1px solid #d4d4d8}button{background:#18181b;color:#fff}</style>
<form method="post" action="/auth/dev/login">
<strong>Dev login</strong><small>Development only: signs in as whoever you type.</small>
<input name="name" placeholder="Your name" maxlength="32" required autofocus>
<input type="hidden" name="next" value="{{.}}">
<button>Sign in</button>
</form>`))

func (s *Service) devForm(w http.ResponseWriter, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = devFormTmpl.Execute(w, next)
}

func (s *Service) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DevLogin {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || utf8.RuneCountInString(name) > 32 {
		http.Error(w, "name must be 1 to 32 characters", http.StatusBadRequest)
		return
	}
	// The name is the identity: signing in as "Ayşe" twice is the same user.
	if err := s.signIn(w, r, devProviderID, Profile{Subject: strings.ToLower(name), Name: name}); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

// ---- invites ---------------------------------------------------------------

var boardRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (s *Service) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	board := r.PathValue("board")
	if !boardRe.MatchString(board) {
		http.Error(w, "bad board name", http.StatusBadRequest)
		return
	}
	u, err := s.currentUser(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var body struct {
		Role proto.Role `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil ||
		(body.Role != proto.RoleEditor && body.Role != proto.RoleViewer) {
		http.Error(w, `role must be "editor" or "viewer"`, http.StatusBadRequest)
		return
	}
	role, err := s.store.ClaimBoard(r.Context(), board, u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if role != proto.RoleOwner {
		http.Error(w, "only the board owner can invite", http.StatusForbidden)
		return
	}
	code, hash := newInviteCode()
	expires := s.cfg.Now().Add(s.cfg.InviteTTL)
	if err := s.store.CreateInvite(r.Context(), hash, board, body.Role, u.ID, expires); err != nil {
		s.fail(w, err)
		return
	}
	link := s.cfg.PublicURL + "/?room=" + url.QueryEscape(board) + "&invite=" + code
	writeJSON(w, http.StatusCreated, map[string]any{"token": code, "code": code, "url": link, "role": body.Role, "expires": expires})
}

func (s *Service) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	u, err := s.currentUser(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	board, role, err := s.store.AcceptInvite(r.Context(), hashToken(normalizeInviteCode(r.PathValue("token"))), u.ID, s.cfg.Now())
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "this invite link is invalid or has expired", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"board": board, "role": role})
}

// ---- helpers -----------------------------------------------------------------

// sameOrigin rejects state-changing requests from other sites. SameSite
// cookies already stop most of them; this covers older browsers too.
func (s *Service) sameOrigin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			u, err := url.Parse(origin)
			public, _ := url.Parse(s.cfg.PublicURL)
			if err != nil || (u.Host != r.Host && (public == nil || u.Host != public.Host)) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		h(w, r)
	}
}

func (s *Service) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	s.log.Error("auth request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// safeNext keeps post-login redirects on this site: a path, never a
// scheme-relative or absolute URL.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
