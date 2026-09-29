package cluster_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/goleak"

	"github.com/highlvmami/lumoraboard/backend/internal/cluster"
	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/room"
	"github.com/highlvmami/lumoraboard/backend/internal/store"
	"github.com/highlvmami/lumoraboard/backend/internal/ws"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var secret = []byte("test-cluster-secret-0123456789")

const ttl = 600 * time.Millisecond

// instance is one server: hub, lease node and socket handler.
type instance struct {
	name  string
	node  *cluster.Node
	srv   *httptest.Server
	stop  func() // graceful: flush, then release leases
	crash func() // abrupt: connections cut, leases left to expire
}

func startInstance(t *testing.T, name string, st *store.Memory) *instance {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	in := &instance{name: name}
	// The handler is built before the server exists, so the node learns
	// its address through a late-bound mux.
	mux := http.NewServeMux()
	in.srv = httptest.NewUnstartedServer(mux)
	conns := &tracker{Listener: in.srv.Listener}
	in.srv.Listener = conns
	in.srv.Start()
	in.node = cluster.New(cluster.Config{Instance: name, Addr: in.srv.URL, TTL: ttl}, st, log)
	hub := room.NewHub(room.Config{
		InboundBuffer: 16, IdleTimeout: time.Hour, OpLogSize: 100,
		Store: st, FlushInterval: 5 * time.Millisecond, Owner: in.node,
	}, log)
	in.node.Attach(hub)
	cfg := ws.DefaultConfig()
	mux.Handle("/ws", ws.NewHandler(hub, cfg, log).WithCluster(in.node, secret))

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = hub.Run(ctx) })
	wg.Go(func() { _ = in.node.Run(ctx) })
	var once sync.Once
	in.stop = func() {
		once.Do(func() {
			in.srv.CloseClientConnections()
			cancel()
			wg.Wait()
			in.node.Release()
			in.srv.Close()
		})
	}
	in.crash = func() {
		once.Do(func() {
			// Cut every socket first, as a dead process would, and never
			// release: the others must wait for the leases to run out.
			// httptest forgets hijacked (WebSocket) connections, so the
			// listener tracks them.
			conns.cut()
			in.srv.Close()
			cancel()
			wg.Wait()
		})
	}
	t.Cleanup(in.stop)
	return in
}

// tracker remembers every accepted connection so a test can cut them all.
type tracker struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *tracker) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *tracker) cut() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		_ = c.Close()
	}
}

type client struct {
	t    *testing.T
	conn *websocket.Conn
	seq  uint64
}

// dial connects to in and waits for the hello, retrying while the room
// is between owners (close 1012/1013), as the browser does.
func dial(t *testing.T, in *instance, board string, since uint64) (*client, proto.Hello) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, h, err := tryDial(in, board, since)
		if err == nil {
			t.Cleanup(func() { _ = c.CloseNow() })
			return &client{t: t, conn: c, seq: h.Seq}, h
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", in.name, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func tryDial(in *instance, board string, since uint64) (*websocket.Conn, proto.Hello, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(in.srv.URL, "http") + "/ws?room=" + board + fmt.Sprintf("&since=%d", since)
	c, resp, err := websocket.Dial(ctx, url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, proto.Hello{}, err
	}
	c.SetReadLimit(1 << 20)
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			_ = c.CloseNow()
			return nil, proto.Hello{}, err
		}
		var env proto.Envelope
		_ = json.Unmarshal(data, &env)
		if env.Type == proto.TypeHello {
			var h proto.Hello
			_ = json.Unmarshal(env.Payload, &h)
			return c, h, nil
		}
	}
}

func (c *client) send(id string) {
	c.t.Helper()
	msg := fmt.Sprintf(`{"v":1,"type":"op","clientOpId":%q,"payload":{"kind":"add","id":%q,"object":{"id":%q,"kind":"rect"}}}`, id, id, id)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		c.t.Fatal(err)
	}
}

// op reads until the next op and returns its seq and client op id.
func (c *client) op() (uint64, string) {
	c.t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, data, err := c.conn.Read(ctx)
		cancel()
		if err != nil {
			c.t.Fatalf("waiting for op: %v", err)
		}
		var env proto.Envelope
		_ = json.Unmarshal(data, &env)
		if env.Type == proto.TypeOp {
			c.seq = env.Seq
			return env.Seq, env.ClientOpID
		}
	}
}

// closed waits for the connection to end and returns its close code.
func (c *client) closed() websocket.StatusCode {
	c.t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _, err := c.conn.Read(ctx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				c.t.Fatal("connection stayed open")
			}
			return websocket.CloseStatus(err)
		}
	}
}

func TestClientsOnTwoInstancesShareARoom(t *testing.T) {
	st := store.NewMemory()
	a := startInstance(t, "a", st)
	b := startInstance(t, "b", st)

	onA, _ := dial(t, a, "r", 0)
	onB, hello := dial(t, b, "r", 0)
	if len(hello.Members) != 1 {
		t.Fatalf("b's client sees %d members, want the one on a", len(hello.Members))
	}

	onA.send("x")
	onB.send("y")
	for _, c := range []*client{onA, onB} {
		s1, id1 := c.op()
		s2, id2 := c.op()
		if s1 != 1 || s2 != 2 || id1 != "x" || id2 != "y" {
			t.Fatalf("got %d:%s %d:%s, want one order on both instances", s1, id1, s2, id2)
		}
	}
}

func TestBoardSurvivesItsOwnerCrashing(t *testing.T) {
	st := store.NewMemory()
	a := startInstance(t, "a", st)
	b := startInstance(t, "b", st)

	onA, _ := dial(t, a, "r", 0) // a owns the room
	onB, _ := dial(t, b, "r", 0) // forwarded to a
	onA.send("x")
	onA.op()
	onB.op()
	time.Sleep(50 * time.Millisecond) // let the write-behind flush

	a.crash()
	// b's client is told to reconnect rather than seeing a dead socket.
	if code := onB.closed(); code != websocket.StatusServiceRestart {
		t.Fatalf("forwarded client closed with %d, want 1012", code)
	}

	// Its retry lands on b, which takes the board over once a's lease
	// runs out, loaded from the store.
	start := time.Now()
	again, hello := dial(t, b, "r", onB.seq)
	if hello.Seq != 1 {
		t.Fatalf("after takeover seq = %d, want 1", hello.Seq)
	}
	if waited := time.Since(start); waited > 3*ttl {
		t.Fatalf("takeover took %v with a %v lease", waited, ttl)
	}
	again.send("y")
	if seq, _ := again.op(); seq != 2 {
		t.Fatalf("next op seq = %d, want 2", seq)
	}
}

func TestGracefulStopHandsOverAtOnce(t *testing.T) {
	st := store.NewMemory()
	a := startInstance(t, "a", st)
	b := startInstance(t, "b", st)
	onA, _ := dial(t, a, "r", 0)
	onA.send("x")
	onA.op()

	a.stop()
	start := time.Now()
	c, hello := dial(t, b, "r", 0)
	if hello.Seq != 1 {
		t.Fatalf("seq %d after handover", hello.Seq)
	}
	if time.Since(start) > ttl/2 {
		t.Fatalf("released lease still took %v", time.Since(start))
	}
	_ = c
}

func TestLostLeaseClosesTheRoom(t *testing.T) {
	st := store.NewMemory()
	a := startInstance(t, "a", st)
	onA, _ := dial(t, a, "r", 0)

	// Another instance steals the lease behind a's back, as after a long
	// pause of a. a's next renewal notices and closes the room.
	ctx := context.Background()
	if err := st.Release(ctx, "r", "a"); err != nil {
		t.Fatal(err)
	}
	if l, err := st.Acquire(ctx, "r", "z", "http://z", time.Minute); err != nil || l.Instance != "z" {
		t.Fatalf("steal: %+v %v", l, err)
	}
	if code := onA.closed(); code != websocket.StatusServiceRestart {
		t.Fatalf("close code %d, want 1012", code)
	}
}

func TestForgedForwardIsRefused(t *testing.T) {
	st := store.NewMemory()
	a := startInstance(t, "a", st)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("X-Lumora-Forward", "eyJ1c2VyIjoiYWRtaW4ifQ.bm9wZQ")
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(a.srv.URL, "http")+"/ws?room=r", &websocket.DialOptions{HTTPHeader: hdr})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged forward: %v %v", resp, err)
	}
}
