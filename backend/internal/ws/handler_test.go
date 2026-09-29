package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/goleak"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/room"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

type fixture struct {
	url string
	hub *room.Hub
}

func newFixture(t *testing.T, cfg Config) fixture {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := room.NewHub(room.Config{InboundBuffer: 8, IdleTimeout: time.Hour, OpLogSize: 100}, log)

	ctx, cancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		_ = hub.Run(ctx)
	}()

	// /ws uses cfg; /ws-slow is the same hub with a one-message outbox, so
	// a test can pair a normal client with one that is easy to overflow.
	slowCfg := cfg
	slowCfg.SendBuffer = 1
	mux := http.NewServeMux()
	mux.Handle("/ws", NewHandler(hub, cfg, log))
	mux.Handle("/ws-slow", NewHandler(hub, slowCfg, log))
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		cancel()
		<-hubDone
		srv.CloseClientConnections()
		srv.Close()
	})
	return fixture{url: "ws" + strings.TrimPrefix(srv.URL, "http"), hub: hub}
}

func dial(t *testing.T, f fixture, roomName string) *websocket.Conn {
	return dialPath(t, f, "/ws", roomName)
}

func dialPath(t *testing.T, f fixture, path, roomName string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, f.url+path+"?room="+roomName, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func read(t *testing.T, conn *websocket.Conn, typ string) proto.Envelope {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, data, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read waiting for %q: %v", typ, err)
		}
		var env proto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatal(err)
		}
		if env.Type == typ {
			return env
		}
	}
}

func send(t *testing.T, conn *websocket.Conn, raw string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestTwoClientsExchangeOps(t *testing.T) {
	f := newFixture(t, DefaultConfig())
	a := dial(t, f, "r1")
	// The upgrade returns before the join; wait for a's hello so a is in
	// the room when b arrives.
	read(t, a, proto.TypeHello)
	b := dial(t, f, "r1")

	helloB := read(t, b, proto.TypeHello)
	var hb proto.Hello
	if err := json.Unmarshal(helloB.Payload, &hb); err != nil {
		t.Fatal(err)
	}
	if len(hb.Members) != 1 {
		t.Fatalf("b's hello lists %d members, want 1", len(hb.Members))
	}

	send(t, a, `{"v":1,"type":"op","clientOpId":"x","payload":{"kind":"add","id":"r1","object":{"id":"r1","kind":"rect"}}}`)
	for name, c := range map[string]*websocket.Conn{"a": a, "b": b} {
		got := read(t, c, proto.TypeOp)
		if got.Seq != 1 || got.ClientOpID != "x" || got.From == "" {
			t.Fatalf("%s got %+v", name, got)
		}
	}
}

func TestRoomsAreIsolated(t *testing.T) {
	f := newFixture(t, DefaultConfig())
	a := dial(t, f, "r1")
	b := dial(t, f, "r2")
	read(t, a, proto.TypeHello)
	read(t, b, proto.TypeHello)

	send(t, a, `{"v":1,"type":"op","clientOpId":"x","payload":{"kind":"add","id":"r1","object":{"id":"r1","kind":"rect"}}}`)
	read(t, a, proto.TypeOp)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, data, err := b.Read(ctx); err == nil {
		t.Fatalf("room r2 received %s", data)
	}
}

func TestBadEnvelopeClosesConnection(t *testing.T) {
	f := newFixture(t, DefaultConfig())
	a := dial(t, f, "r1")
	read(t, a, proto.TypeHello)

	send(t, a, `{"v":1,"type":"hello"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	for err == nil {
		_, _, err = a.Read(ctx)
	}
	if websocket.CloseStatus(err) != websocket.StatusUnsupportedData {
		t.Fatalf("close status = %v (%v), want UnsupportedData", websocket.CloseStatus(err), err)
	}
}

func TestMalformedOpIsRejectedNotDisconnected(t *testing.T) {
	f := newFixture(t, DefaultConfig())
	a := dial(t, f, "r1")
	read(t, a, proto.TypeHello)

	send(t, a, `{"v":1,"type":"op","clientOpId":"bad","payload":{"kind":"explode","id":"x"}}`)
	rej := read(t, a, proto.TypeReject)
	if rej.ClientOpID != "bad" {
		t.Fatalf("reject = %+v", rej)
	}

	// Still connected: a valid op goes through.
	send(t, a, `{"v":1,"type":"op","clientOpId":"ok","payload":{"kind":"add","id":"r1","object":{"id":"r1","kind":"rect"}}}`)
	if got := read(t, a, proto.TypeOp); got.Seq != 1 {
		t.Fatalf("op after reject = %+v", got)
	}
}

func TestSinceQueryResumes(t *testing.T) {
	f := newFixture(t, DefaultConfig())
	a := dial(t, f, "r1")
	read(t, a, proto.TypeHello)
	send(t, a, `{"v":1,"type":"op","clientOpId":"1","payload":{"kind":"add","id":"r1","object":{"id":"r1","kind":"rect"}}}`)
	send(t, a, `{"v":1,"type":"op","clientOpId":"2","payload":{"kind":"add","id":"r2","object":{"id":"r2","kind":"rect"}}}`)
	read(t, a, proto.TypeOp)
	read(t, a, proto.TypeOp)

	b := dial(t, f, "r1&since=1")
	hello := read(t, b, proto.TypeHello)
	var h proto.Hello
	if err := json.Unmarshal(hello.Payload, &h); err != nil {
		t.Fatal(err)
	}
	if !h.Resume {
		t.Fatalf("hello = %+v, want resume", h)
	}
	if got := read(t, b, proto.TypeOp); got.Seq != 2 || got.ClientOpID != "2" {
		t.Fatalf("replayed %+v", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, resp, err := websocket.Dial(ctx, f.url+"/ws?room=r1&since=abc", nil); err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad since accepted: err=%v resp=%v", err, resp)
	} else {
		_ = resp.Body.Close()
	}
}

func TestBadRoomNameIsRejected(t *testing.T) {
	f := newFixture(t, DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, f.url+"/ws?room=bad%20name", nil)
	if err == nil {
		t.Fatal("dial succeeded with an invalid room name")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("response = %v, want 400", resp)
	}
	_ = resp.Body.Close()
}

func TestSlowConsumerGetsPolicyViolation(t *testing.T) {
	f := newFixture(t, DefaultConfig())

	sender := dial(t, f, "r1")
	read(t, sender, proto.TypeHello)
	slow := dialPath(t, f, "/ws-slow", "r1")
	read(t, slow, proto.TypeHello)

	// The sender keeps draining its own echoes so it is never the slow one.
	sawLeft := make(chan string, 1)
	drainCtx, stopDrain := context.WithCancel(context.Background())
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			_, data, err := sender.Read(drainCtx)
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeLeft {
				select {
				case sawLeft <- env.From:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		stopDrain()
		<-drained
	})

	// slow never reads again. Its write pump keeps writing until the
	// kernel socket buffers fill; from then on it blocks, the one-slot
	// outbox overflows on the next broadcast and the room drops it. Big
	// ops get there quickly whatever the buffer sizes are.
	points := strings.Repeat(`{"x":123456.5,"y":654321.5},`, 299) + `{"x":1,"y":1}`
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; f.hub.SlowDrops() == 0; i++ {
		if time.Now().After(deadline) {
			t.Fatal("slow consumer was never dropped")
		}
		// A fresh stroke each time, so every op is accepted and broadcast.
		send(t, sender, fmt.Sprintf(`{"v":1,"type":"op","clientOpId":"x","payload":{"kind":"add","id":"s%d","object":{"id":"s%d","kind":"stroke","points":[%s]}}}`, i, i, points))
	}

	select {
	case from := <-sawLeft:
		if from == "" {
			t.Fatal("left notice has no from")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sender was not told the slow client left")
	}
	if f.hub.SlowDrops() != 1 {
		t.Fatalf("SlowDrops = %d, want 1", f.hub.SlowDrops())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	for err == nil {
		_, _, err = slow.Read(ctx)
	}
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("slow close status = %v (%v), want PolicyViolation", websocket.CloseStatus(err), err)
	}
}

func TestCursorReachesOthersButNotSender(t *testing.T) {
	f := newFixture(t, DefaultConfig())
	a := dialPath(t, f, "/ws", "r1&name=Ay%C5%9Fe")
	read(t, a, proto.TypeHello)
	b := dialPath(t, f, "/ws", "r1&name=%20%20Bora%07%20")

	var hb proto.Hello
	if err := json.Unmarshal(read(t, b, proto.TypeHello).Payload, &hb); err != nil {
		t.Fatal(err)
	}
	if len(hb.Members) != 1 || hb.Members[0].Name != "Ayşe" {
		t.Fatalf("b's hello members = %+v, want Ayşe", hb.Members)
	}
	var joined proto.Member
	if err := json.Unmarshal(read(t, a, proto.TypeJoined).Payload, &joined); err != nil {
		t.Fatal(err)
	}
	if joined.Name != "Bora" {
		t.Fatalf("joined = %+v, want cleaned name Bora", joined)
	}

	send(t, b, `{"v":1,"type":"cursor","payload":{"x":10,"y":20}}`)
	got := read(t, a, proto.TypeCursor)
	var cur proto.Cursor
	if err := json.Unmarshal(got.Payload, &cur); err != nil {
		t.Fatal(err)
	}
	if got.From != joined.ID || cur.X != 10 || cur.Y != 20 || got.Seq != 0 {
		t.Fatalf("cursor = %+v %+v", got, cur)
	}

	// The sender never sees its own cursor: an op sent after it is the
	// next thing b reads.
	send(t, b, `{"v":1,"type":"op","clientOpId":"x","payload":{"kind":"add","id":"r1","object":{"id":"r1","kind":"rect"}}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := b.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var env proto.Envelope
	if err := json.Unmarshal(data, &env); err != nil || env.Type != proto.TypeOp {
		t.Fatalf("b read %s, want its op echo", data)
	}
}

func TestCursorFloodIsThrottled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CursorInterval = time.Hour
	f := newFixture(t, cfg)
	a := dial(t, f, "r1")
	read(t, a, proto.TypeHello)
	b := dial(t, f, "r1")
	read(t, b, proto.TypeHello)

	for i := range 50 {
		send(t, b, fmt.Sprintf(`{"v":1,"type":"cursor","payload":{"x":%d,"y":0}}`, i))
	}
	send(t, b, `{"v":1,"type":"cursor","payload":{"hidden":true}}`)

	first := read(t, a, proto.TypeCursor)
	var cur proto.Cursor
	_ = json.Unmarshal(first.Payload, &cur)
	if cur.X != 0 || cur.Hidden {
		t.Fatalf("first cursor = %+v, want x=0", cur)
	}
	// Everything in between fell inside the interval; hidden is exempt.
	second := read(t, a, proto.TypeCursor)
	_ = json.Unmarshal(second.Payload, &cur)
	if !cur.Hidden {
		t.Fatalf("second cursor = %+v, want hidden", cur)
	}
}

type fakeAuth struct {
	id  Identity
	err error
}

func (f fakeAuth) Authorize(*http.Request, string) (Identity, error) { return f.id, f.err }

func dialWithAuth(t *testing.T, a Authorizer) *websocket.Conn {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := room.NewHub(room.Config{InboundBuffer: 8, IdleTimeout: time.Hour, OpLogSize: 100}, log)
	ctx, cancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		_ = hub.Run(ctx)
	}()
	srv := httptest.NewServer(NewHandler(hub, DefaultConfig(), log).WithAuth(a))
	t.Cleanup(func() {
		cancel()
		<-hubDone
		srv.CloseClientConnections()
		srv.Close()
	})
	dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dcancel()
	conn, resp, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"?room=r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func TestAuthFailuresCloseWithAppCodes(t *testing.T) {
	for want, err := range map[websocket.StatusCode]error{4401: ErrUnauthorized, 4403: ErrForbidden} {
		conn := dialWithAuth(t, fakeAuth{err: err})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, rerr := conn.Read(ctx)
		cancel()
		if websocket.CloseStatus(rerr) != want {
			t.Fatalf("%v: close status = %v (%v), want %d", err, websocket.CloseStatus(rerr), rerr, want)
		}
	}
}

func TestViewerCanWatchButNotDraw(t *testing.T) {
	conn := dialWithAuth(t, fakeAuth{id: Identity{User: "u1", Name: "Vera", Role: proto.RoleViewer}})
	var h proto.Hello
	if err := json.Unmarshal(read(t, conn, proto.TypeHello).Payload, &h); err != nil {
		t.Fatal(err)
	}
	if h.Role != proto.RoleViewer {
		t.Fatalf("hello role = %q", h.Role)
	}
	send(t, conn, `{"v":1,"type":"op","clientOpId":"x","payload":{"kind":"add","id":"r1","object":{"id":"r1","kind":"rect"}}}`)
	rej := read(t, conn, proto.TypeReject)
	var p proto.Reject
	_ = json.Unmarshal(rej.Payload, &p)
	if rej.ClientOpID != "x" || !strings.Contains(p.Reason, "read-only") {
		t.Fatalf("reject = %+v %+v", rej, p)
	}
}

// A client that answers pings stays connected however long it stays
// silent: someone watching a board sends nothing for minutes.
func TestQuietClientStaysConnected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReadTimeout = 200 * time.Millisecond
	cfg.WriteTimeout = 5 * time.Second
	f := newFixture(t, cfg)
	watcher := dial(t, f, "quiet")
	read(t, watcher, proto.TypeHello)
	drawer := dial(t, f, "quiet")
	read(t, drawer, proto.TypeHello)

	go func() {
		time.Sleep(5 * cfg.ReadTimeout)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = drawer.Write(ctx, websocket.MessageText, []byte(`{"v":1,"type":"op","clientOpId":"x","payload":{"kind":"add","id":"q","object":{"id":"q","kind":"rect"}}}`))
	}()
	// The watcher sends nothing; reading keeps answering the server's pings.
	if got := read(t, watcher, proto.TypeOp); got.Seq != 1 {
		t.Fatalf("got %+v", got)
	}
}
