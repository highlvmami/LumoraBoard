package ws

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

// ChatHistory serves older chat messages over REST. The socket only
// carries the latest ones (in hello); scrolling further back pages here.
type ChatHistory struct {
	store store.Store
	auth  Authorizer
	log   *slog.Logger
}

// NewChatHistory builds the endpoint. auth may be nil in open mode.
func NewChatHistory(st store.Store, auth Authorizer, log *slog.Logger) *ChatHistory {
	return &ChatHistory{store: st, auth: auth, log: log}
}

// Register adds GET /api/boards/{board}/chat?before=<id>&limit=<n>.
func (c *ChatHistory) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/boards/{board}/chat", c.serve)
}

const (
	chatPageDefault = 50
	chatPageMax     = 100
)

func (c *ChatHistory) serve(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("board")
	if !roomNameRe.MatchString(name) {
		http.Error(w, "bad board name", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	var before uint64
	if raw := q.Get("before"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			http.Error(w, "before must be a non-negative integer", http.StatusBadRequest)
			return
		}
		before = v
	}
	limit := chatPageDefault
	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = min(v, chatPageMax)
	}

	if c.auth != nil {
		_, err := c.auth.Authorize(r, name)
		switch {
		case errors.Is(err, ErrUnauthorized):
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		case errors.Is(err, ErrForbidden):
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		case err != nil:
			c.log.Error("authorize failed", "err", err)
			http.Error(w, "try again later", http.StatusInternalServerError)
			return
		}
	}

	msgs, more, err := c.store.ChatBefore(r.Context(), name, before, limit)
	if err != nil {
		c.log.Error("chat history failed", "board", name, "err", err)
		http.Error(w, "try again later", http.StatusServiceUnavailable)
		return
	}
	if msgs == nil {
		msgs = []proto.ChatMessage{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(proto.ChatHistory{Messages: msgs, More: more})
}
