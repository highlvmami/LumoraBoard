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
