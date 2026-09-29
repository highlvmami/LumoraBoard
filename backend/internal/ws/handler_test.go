package ws

import (
	"context"
	"encoding/json"
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
	hub := room.NewHub(room.Config{InboundBuffer: 8, IdleTimeout: time.Hour}, log)

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
	b := dial(t, f, "r1")

	helloB := read(t, b, proto.TypeHello)
	var hb proto.Hello
	if err := json.Unmarshal(helloB.Payload, &hb); err != nil {
		t.Fatal(err)
	}
	if len(hb.Members) != 1 {
		t.Fatalf("b's hello lists %d members, want 1", len(hb.Members))
	}

	send(t, a, `{"v":1,"type":"op","clientOpId":"x","payload":{"k":"stroke"}}`)
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

	send(t, a, `{"v":1,"type":"op","clientOpId":"x"}`)
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

	// slow never reads again. Its write pump moves a message or two into
	// the kernel buffer, then the room finds the outbox full and drops it.
	for range 200 {
		send(t, sender, `{"v":1,"type":"op","clientOpId":"x"}`)
	}

	select {
	case from := <-sawLeft:
		if from == "" {
			t.Fatal("left notice has no from")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow consumer was never dropped")
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
