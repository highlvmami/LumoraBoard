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
	// ReadTimeout sets the keepalive: pings go out at half this interval
	// and a client that does not answer one within WriteTimeout is
	// dropped. Silence alone is fine; a viewer may send nothing for hours.
	ReadTimeout time.Duration
	// WriteTimeout bounds a single outbound write.
	WriteTimeout time.Duration
	// HandshakeTimeout bounds authorizing and joining a room.
	HandshakeTimeout time.Duration
	// CursorInterval is the minimum gap between two cursor updates from
	// one connection; faster ones are dropped. Clients throttle to about
	// 25 Hz on their own, so this only bites on misbehaving ones.
	CursorInterval time.Duration
}

// DefaultConfig is what the server uses unless told otherwise.
func DefaultConfig() Config {
	return Config{
		SendBuffer:       256,
		MaxMessageBytes:  64 << 10,
		ReadTimeout:      60 * time.Second,
		WriteTimeout:     10 * time.Second,
		HandshakeTimeout: 10 * time.Second,
		CursorInterval:   25 * time.Millisecond,
	}
}

var roomNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Identity is who a connection belongs to and what it may do.
type Identity struct {
	User   string // account id; empty for guests and open mode
	Name   string
	Avatar string
	Role   proto.Role
}

// Authorizer decides who is behind an upgrade request and their role on
// the board. It runs before the room is joined.
type Authorizer interface {
	Authorize(r *http.Request, board string) (Identity, error)
}

// Authorization failures. The socket is accepted and then closed with
// these codes (4000-4999 are for applications) because a browser cannot
// read the HTTP status of a failed upgrade.
var (
	ErrUnauthorized = errors.New("sign in required")
	ErrForbidden    = errors.New("no access to this board")
)

const (
	closeUnauthorized websocket.StatusCode = 4401
	closeForbidden    websocket.StatusCode = 4403
)

// Handler upgrades requests on its route and attaches them to the hub.
type Handler struct {
	hub  *room.Hub
	cfg  Config
	log  *slog.Logger
	auth Authorizer

	cluster Cluster // nil on a single server
	secret  []byte
}

// NewHandler builds the handler.
func NewHandler(hub *room.Hub, cfg Config, log *slog.Logger) *Handler {
	return &Handler{hub: hub, cfg: cfg, log: log}
}

// WithAuth makes every connection go through a. Without it everyone is
// an editor and picks their own display name with ?name=.
func (h *Handler) WithAuth(a Authorizer) *Handler {
	h.auth = a
	return h
}

// maxNameRunes caps the display name a client may pick.
const maxNameRunes = 32

// ServeHTTP expects ?room=<name>, optionally &since=<seq> (the last seq a
// reconnecting client saw) and &name=<display name>, and serves the connection until either side
// closes it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("room")
	// Every step up to the join is logged at Info with one trace id, so
	// on a host with only logs to go by a stalled socket shows its last
	// step. Each blocking step is also bounded.
	tl := h.log.With("trace", newClientID()[:8], "room", name)
	began := time.Now()
	elapsed := func() string { return time.Since(began).Round(time.Millisecond).String() }
	if !roomNameRe.MatchString(name) {
		tl.Warn("ws refused: bad room name")
		http.Error(w, "room must match "+roomNameRe.String(), http.StatusBadRequest)
		return
	}
	var since uint64
	if raw := r.URL.Query().Get("since"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			tl.Warn("ws refused: bad since", "since", raw)
			http.Error(w, "since must be a non-negative integer", http.StatusBadRequest)
			return
		}
		since = v
	}

	id := Identity{Role: proto.RoleEditor, Name: cleanName(r.URL.Query().Get("name"))}
	var authErr error
	// Another instance forwarding a client has checked it already and
	// signed who it is.
	fid, isForwarded, fwdErr := readForward(h.secret, r, time.Now())
	switch {
	case fwdErr != nil:
		tl.Warn("ws refused: bad forward header", "err", fwdErr)
		http.Error(w, fwdErr.Error(), http.StatusForbidden)
		return
	case isForwarded:
		id = fid
	case h.auth != nil:
		// Bounded: a stuck database must fail the upgrade, not hang it.
		tl.Info("ws authorizing")
		actx, cancel := context.WithTimeout(r.Context(), h.cfg.HandshakeTimeout)
		id, authErr = h.auth.Authorize(r.WithContext(actx), name)
		cancel()
		tl.Info("ws authorized", "took", elapsed(), "user", id.User, "role", id.Role, "err", authErr)
		id.Name = cleanName(id.Name)
	}

	// Origin is checked here, before any auth answer goes out.
	tl.Info("ws accepting", "forwarded", isForwarded)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.cfg.OriginPatterns})
	if err != nil {
		// Usually an origin the server does not allow; the browser only
		// sees the socket close, so this log line is the explanation.
		h.log.Warn("websocket handshake rejected", "err", err, "origin", r.Header.Get("Origin"), "host", r.Host)
		return
	}
	conn.SetReadLimit(h.cfg.MaxMessageBytes)
	tl.Info("ws accepted", "took", elapsed())
	if authErr != nil {
		tl.Info("ws closing: not authorized", "err", authErr)
	}
	switch {
	case errors.Is(authErr, ErrUnauthorized):
		_ = conn.Close(closeUnauthorized, authErr.Error())
		return
	case errors.Is(authErr, ErrForbidden):
		_ = conn.Close(closeForbidden, authErr.Error())
		return
	case authErr != nil:
		h.log.Error("authorize failed", "err", authErr)
		_ = conn.Close(websocket.StatusInternalError, "try again later")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	if h.cluster != nil {
		tl.Info("ws resolving owner")
		octx, ocancel := context.WithTimeout(ctx, h.cfg.HandshakeTimeout)
		lease, self, err := h.cluster.Owner(octx, name)
		ocancel()
		tl.Info("ws owner resolved", "took", elapsed(), "owner", lease.Instance, "self", self, "err", err)
		switch {
		case err != nil:
			h.log.Error("could not resolve room owner", "room", name, "err", err)
			_ = conn.Close(websocket.StatusTryAgainLater, "room unavailable")
			return
		case !self && isForwarded:
			// Ownership changed while the client was being forwarded;
			// its retry resolves again instead of bouncing around.
			_ = conn.Close(websocket.StatusTryAgainLater, "room moving")
			return
		case !self:
			tl.Info("ws forwarding", "owner", lease.Instance, "addr", lease.Addr)
			err := h.proxy(ctx, conn, lease.Addr, forwardParams(r, name, since), id)
			tl.Info("forwarded connection ended", "room", name, "owner", lease.Instance, "err", err)
			return
		}
	}

	client := room.NewClient(newClientID(), h.cfg.SendBuffer).
		WithName(id.Name).
		WithAccount(id.User, id.Avatar, id.Role)
	tl.Info("joining room", "user", id.User, "took", elapsed())
	// Bounded like authorizing: a board that cannot load must end in a
	// logged error and a retry, not a socket that never says hello.
	jctx, jcancel := context.WithTimeout(ctx, 3*h.cfg.HandshakeTimeout)
	rm, err := h.hub.Join(jctx, name, client, since)
	jcancel()
	if err != nil {
		// 1013: the client's backoff retries, which is right for both a
		// stopping hub and a board the store could not load yet.
		tl.Warn("join failed", "took", elapsed(), "err", err)
		_ = conn.Close(websocket.StatusTryAgainLater, "room unavailable")
		return
	}
	log := h.log.With("room", name, "client", client.ID())
	// Info on purpose: on a host with only logs to go by, these two lines
	// show whether sockets reach the server and why they end.
	start := time.Now()
	log.Info("socket connected", "user", id.User, "took", elapsed())

	writeDone, readDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(writeDone)
		if h.writePump(ctx, conn, client, readDone) {
			// The room dropped us: say why with a proper close frame,
			// which also unblocks the read pump.
			h.closeConn(conn, client, nil)
			return
		}
		cancel() // a write failed; tear the connection down
	}()

	err = h.readPump(ctx, conn, client, rm)
	close(readDone)
	rm.Leave(client)
	<-writeDone

	h.closeConn(conn, client, err)
	log.Info("socket closed", "after", time.Since(start).Round(time.Second).String(), "reason", client.Reason(), "err", err)
}

// readPump decodes client frames and hands them to the room. It returns
// when the connection fails, the client misbehaves, or ctx is cancelled.
func (h *Handler) readPump(ctx context.Context, conn *websocket.Conn, client *room.Client, rm *room.Room) error {
	var lastCursor, lastTyping time.Time
	for {
		// No deadline here: the write pump's pings detect a dead peer, and
		// a failed ping cancels ctx.
		_, data, err := conn.Read(ctx)
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
		switch env.Type {
		case proto.TypeChatTyping:
			if now := time.Now(); now.Sub(lastTyping) >= typingInterval {
				lastTyping = now
				rm.Typing(client)
			}
			continue
		case proto.TypeChatSend:
			msg, err := proto.DecodeChat(env.Payload)
			if err != nil {
				h.rejectFrame(client, rm, env, err)
				continue
			}
			if err := rm.SubmitChat(ctx, client, env, msg); err != nil {
				return err
			}
			continue
		}
		op, err := board.DecodeOp(env.Payload)
		if err != nil {
			// A malformed op is the sender's problem alone: tell them and
			// keep the connection; only a broken envelope closes it.
			h.rejectFrame(client, rm, env, err)
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
func (h *Handler) writePump(ctx context.Context, conn *websocket.Conn, client *room.Client, readDone <-chan struct{}) bool {
	ping := time.NewTicker(h.cfg.ReadTimeout / 2)
	defer ping.Stop()
	// Once the read pump is gone nothing will read the pong, so a ping in
	// flight must not wait for it.
	pingBase, stopPings := context.WithCancel(ctx)
	defer stopPings()
	go func() {
		select {
		case <-readDone:
			stopPings()
		case <-pingBase.Done():
		}
	}()

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
			pingCtx, cancel := context.WithTimeout(pingBase, h.cfg.WriteTimeout)
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
	case client.Reason() == room.ReasonMoved:
		// 1012: reconnect; the new owner has the board.
		_ = conn.Close(websocket.StatusServiceRestart, string(room.ReasonMoved))
	case errors.Is(readErr, proto.ErrBadEnvelope):
		_ = conn.Close(websocket.StatusUnsupportedData, readErr.Error())
	default:
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
}

// typingInterval is how often one connection may announce typing. Clients
// show the indicator for a few seconds, so once a second is plenty.
const typingInterval = time.Second

// rejectFrame answers a frame that decoded as an envelope but whose
// payload was bad. It is the sender's problem alone, so the connection
// stays open.
func (h *Handler) rejectFrame(client *room.Client, rm *room.Room, env proto.Envelope, err error) {
	client.Deliver(proto.Encode(proto.Envelope{
		V:          proto.Version,
		Type:       proto.TypeReject,
		Room:       rm.Name(),
		ClientOpID: env.ClientOpID,
		Payload:    rejectPayload(env.ClientOpID, err),
	}))
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
