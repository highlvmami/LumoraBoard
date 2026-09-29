// Package ws is the WebSocket transport: it turns one HTTP upgrade into a
// room.Client, then runs a read pump and a write pump for that connection.
package ws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/room"
)

// Config tunes per-connection limits.
type Config struct {
	// OriginPatterns lists hosts allowed to open a socket (see
	// websocket.AcceptOptions). Empty means same origin only.
	OriginPatterns []string
	// SendBuffer is the per-client outbox size in messages.
	SendBuffer int
	// MaxMessageBytes caps a single inbound frame.
	MaxMessageBytes int64
	// ReadTimeout is how long a client may stay silent before it is
	// considered dead. Pings go out at half this interval.
	ReadTimeout time.Duration
	// WriteTimeout bounds a single outbound write.
	WriteTimeout time.Duration
	// CursorInterval is the minimum gap between two cursor updates from
	// one connection; faster ones are dropped. Clients throttle to about
	// 25 Hz on their own, so this only bites on misbehaving ones.
	CursorInterval time.Duration
}

// DefaultConfig is what the server uses unless told otherwise.
func DefaultConfig() Config {
	return Config{
		SendBuffer:      256,
		MaxMessageBytes: 64 << 10,
		ReadTimeout:     60 * time.Second,
		WriteTimeout:    10 * time.Second,
		CursorInterval:  25 * time.Millisecond,
	}
}

var roomNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Handler upgrades requests on its route and attaches them to the hub.
type Handler struct {
	hub *room.Hub
	cfg Config
	log *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(hub *room.Hub, cfg Config, log *slog.Logger) *Handler {
	return &Handler{hub: hub, cfg: cfg, log: log}
}

// maxNameRunes caps the display name a client may pick.
const maxNameRunes = 32

// ServeHTTP expects ?room=<name>, optionally &since=<seq> (the last seq a
// reconnecting client saw) and &name=<display name>, and serves the connection until either side
// closes it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("room")
	if !roomNameRe.MatchString(name) {
		http.Error(w, "room must match "+roomNameRe.String(), http.StatusBadRequest)
		return
	}
	var since uint64
	if raw := r.URL.Query().Get("since"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			http.Error(w, "since must be a non-negative integer", http.StatusBadRequest)
			return
		}
		since = v
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.cfg.OriginPatterns})
	if err != nil {
		h.log.Debug("accept failed", "err", err)
		return
	}
	conn.SetReadLimit(h.cfg.MaxMessageBytes)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	client := room.NewClient(newClientID(), h.cfg.SendBuffer).WithName(cleanName(r.URL.Query().Get("name")))
	rm, err := h.hub.Join(ctx, name, client, since)
	if err != nil {
		_ = conn.Close(websocket.StatusTryAgainLater, "hub unavailable")
		return
	}
	log := h.log.With("room", name, "client", client.ID())
	log.Debug("connected")

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		if h.writePump(ctx, conn, client) {
			// The room dropped us: say why with a proper close frame,
			// which also unblocks the read pump.
			h.closeConn(conn, client, nil)
			return
		}
		cancel() // a write failed; tear the connection down
	}()

	err = h.readPump(ctx, conn, client, rm)
	rm.Leave(client)
	<-writeDone

	h.closeConn(conn, client, err)
	log.Debug("disconnected", "reason", client.Reason(), "err", err)
}

// readPump decodes client frames and hands them to the room. It returns
// when the connection fails, the client misbehaves, or ctx is cancelled.
func (h *Handler) readPump(ctx context.Context, conn *websocket.Conn, client *room.Client, rm *room.Room) error {
	var lastCursor time.Time
	for {
		readCtx, cancel := context.WithTimeout(ctx, h.cfg.ReadTimeout)
		_, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return err
		}

		env, err := proto.Decode(data)
		if err != nil {
			return err
		}
		if env.Type == proto.TypeCursor {
			cur, err := proto.DecodeCursor(env.Payload)
			if err != nil {
				return err
			}
			// Hidden always goes through so a cursor never lingers on
			// other screens after its owner moved away.
			if now := time.Now(); cur.Hidden || now.Sub(lastCursor) >= h.cfg.CursorInterval {
				lastCursor = now
				rm.Cursor(client, cur)
			}
			continue
		}
		op, err := board.DecodeOp(env.Payload)
		if err != nil {
			// A malformed op is the sender's problem alone: tell them and
			// keep the connection; only a broken envelope closes it.
			client.Deliver(proto.Encode(proto.Envelope{
				V:          proto.Version,
				Type:       proto.TypeReject,
				Room:       rm.Name(),
				ClientOpID: env.ClientOpID,
				Payload:    rejectPayload(env.ClientOpID, err),
			}))
			continue
		}
		if err := rm.Submit(ctx, client, env, op); err != nil {
			return err
		}
	}
}

// writePump drains the client's outbox onto the socket and keeps the
// connection alive with pings. It reports true when it stopped because
// the room dropped the client, false when a write failed or ctx ended.
func (h *Handler) writePump(ctx context.Context, conn *websocket.Conn, client *room.Client) bool {
	ping := time.NewTicker(h.cfg.ReadTimeout / 2)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-client.Done():
			return client.Reason() != room.ReasonLeft
		case msg := <-client.Outbox():
			if err := h.write(ctx, conn, msg); err != nil {
				return false
			}
		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return false
			}
		}
	}
}

func (h *Handler) write(ctx context.Context, conn *websocket.Conn, msg []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, msg)
}

// closeConn picks a close status that tells the client what happened.
func (h *Handler) closeConn(conn *websocket.Conn, client *room.Client, readErr error) {
	switch {
	case client.Reason() == room.ReasonSlowConsumer:
		_ = conn.Close(websocket.StatusPolicyViolation, string(room.ReasonSlowConsumer))
	case client.Reason() == room.ReasonShutdown:
		_ = conn.Close(websocket.StatusGoingAway, string(room.ReasonShutdown))
	case errors.Is(readErr, proto.ErrBadEnvelope):
		_ = conn.Close(websocket.StatusUnsupportedData, readErr.Error())
	default:
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
}

func rejectPayload(clientOpID string, err error) json.RawMessage {
	data, _ := json.Marshal(proto.Reject{ClientOpID: clientOpID, Reason: err.Error()})
	return data
}

// cleanName trims a display name, strips control characters and caps its
// length. Names are cosmetic; the client id is what identifies a member.
func cleanName(raw string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(raw) {
		if n == maxNameRunes {
			break
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

func newClientID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ws: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
