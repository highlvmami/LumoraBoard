package export

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/highlvmami/lumoraboard/backend/internal/store"
	"github.com/highlvmami/lumoraboard/backend/internal/ws"
)

var boardRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Handler serves the export and import endpoints.
type Handler struct {
	m     *Manager
	store store.Store
	auth  ws.Authorizer // nil in open mode
	// publicHost is also accepted as an Origin, next to the request's own
	// host, for the dev proxy.
	publicHost string
	log        *slog.Logger
}

// NewHandler builds the endpoints. auth may be nil in open mode.
func NewHandler(m *Manager, st store.Store, auth ws.Authorizer, publicURL string, log *slog.Logger) *Handler {
	h := &Handler{m: m, store: st, auth: auth, log: log}
	if u, err := url.Parse(publicURL); err == nil {
		h.publicHost = u.Host
	}
	return h
}

// Register adds the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/boards/{board}/exports", h.sameOrigin(h.create))
	mux.HandleFunc("GET /api/exports/{id}", h.status)
	mux.HandleFunc("GET /api/exports/{id}/file", h.file)
	mux.HandleFunc("DELETE /api/exports/{id}", h.sameOrigin(h.cancel))
	mux.HandleFunc("POST /api/boards/import", h.sameOrigin(h.importBoard))
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("board")
	if !boardRe.MatchString(name) {
		http.Error(w, "bad board name", http.StatusBadRequest)
		return
	}
	var body struct {
		Format Format  `json:"format"`
		Scale  float64 `json:"scale"`
		Client string  `json:"client"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "body must be JSON", http.StatusBadRequest)
		return
	}
	id, ok := h.authorize(w, r, name)
	if !ok {
		return
	}
	st, err := h.m.Submit(Request{Board: name, Format: body.Format, Scale: body.Scale, User: id.User, Client: body.Client})
	switch {
	case errors.Is(err, ErrBadFormat):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrTooMany):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, ErrBusy), errors.Is(err, ErrClosed):
		w.Header().Set("Retry-After", "5")
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case err != nil:
		h.fail(w, err)
	default:
		writeJSON(w, http.StatusAccepted, st)
	}
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	user, ok := h.jobUser(w, r)
	if !ok {
		return
	}
	st, err := h.m.Status(r.PathValue("id"), user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *Handler) file(w http.ResponseWriter, r *http.Request) {
	user, ok := h.jobUser(w, r)
	if !ok {
		return
	}
	name, f, data, err := h.m.Result(r.PathValue("id"), user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", f.ContentType())
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	user, ok := h.jobUser(w, r)
	if !ok {
		return
	}
	st, err := h.m.Cancel(r.PathValue("id"), user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// jobUser authorizes the caller against the job's board and returns who
// they are. A job the caller may not see is reported as missing.
func (h *Handler) jobUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	name, ok := h.m.Board(r.PathValue("id"))
	if !ok {
		http.Error(w, ErrNotFound.Error(), http.StatusNotFound)
		return "", false
	}
	id, ok := h.authorize(w, r, name)
	return id.User, ok
}

// importBoard creates a new board from a JSON backup and answers with its
// name. The caller owns it: with sign-in on, opening a board nobody owns
// claims it, and that happens here before the board has any content.
func (h *Handler) importBoard(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxBackupBytes+1))
	if err != nil {
		http.Error(w, "could not read the upload", http.StatusBadRequest)
		return
	}
	if len(data) > MaxBackupBytes {
		http.Error(w, "backup is too large", http.StatusRequestEntityTooLarge)
		return
	}
	snap, err := ParseBackup(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name, err := h.freshBoard(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	id, ok := h.authorize(w, r, name)
	if !ok {
		return
	}
	if h.auth != nil && id.User == "" {
		http.Error(w, "sign in to import a board", http.StatusUnauthorized)
		return
	}
	if err := h.store.Compact(r.Context(), name, snap); err != nil {
		h.fail(w, err)
		return
	}
	h.log.Info("board imported", "board", name, "objects", len(snap.Objects), "user", id.User)
	writeJSON(w, http.StatusCreated, map[string]any{"board": name, "objects": len(snap.Objects)})
}

// freshBoard picks an unused board name.
func (h *Handler) freshBoard(ctx context.Context) (string, error) {
	for range 5 {
		name := "import-" + newID()[:10]
		data, err := h.store.Load(ctx, name)
		if err != nil {
			return "", err
		}
		if data.Snapshot.Seq == 0 && len(data.Ops) == 0 {
			return name, nil
		}
	}
	return "", errors.New("could not find a free board name")
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, name string) (ws.Identity, bool) {
	if h.auth == nil {
		return ws.Identity{}, true
	}
	id, err := h.auth.Authorize(r, name)
	switch {
	case errors.Is(err, ws.ErrUnauthorized):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, ws.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case err != nil:
		h.fail(w, err)
	default:
		return id, true
	}
	return ws.Identity{}, false
}

// sameOrigin refuses cross-site requests that change state. Browsers
// always send Origin on those.
func (h *Handler) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || (u.Host != r.Host && u.Host != h.publicHost) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	h.log.Error("export request failed", "err", err)
	http.Error(w, "try again later", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
