package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// startHub runs a hub for the test and stops it, waiting for every room,
// during cleanup. That plus goleak proves shutdown leaks nothing.
func startHub(t *testing.T, cfg Config) *Hub {
	t.Helper()
	h := NewHub(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return h
}

func testCfg() Config {
	return Config{InboundBuffer: 8, IdleTimeout: time.Hour}
}

func op(id string) proto.Envelope {
	return proto.Envelope{V: proto.Version, Type: proto.TypeOp, ClientOpID: id, Payload: json.RawMessage(`{"x":1}`)}
}

// recv reads the next message of the given type, skipping presence noise.
func recv(t *testing.T, c *Client, typ string) proto.Envelope {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case raw := <-c.Outbox():
			var env proto.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("bad message %q: %v", raw, err)
			}
			if env.Type == typ {
				return env
			}
		case <-c.Done():
			t.Fatalf("client %s closed (%s) while waiting for %q", c.ID(), c.Reason(), typ)
		case <-deadline:
			t.Fatalf("client %s: timed out waiting for %q", c.ID(), typ)
		}
	}
}

func TestHelloCarriesMembers(t *testing.T) {
	h := startHub(t, testCfg())
	ctx := context.Background()

	a := NewClient("a", 16)
	if _, err := h.Join(ctx, "r", a); err != nil {
		t.Fatal(err)
	}
	b := NewClient("b", 16)
	if _, err := h.Join(ctx, "r", b); err != nil {
		t.Fatal(err)
	}

	var hello proto.Hello
	if err := json.Unmarshal(recv(t, b, proto.TypeHello).Payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.ClientID != "b" || len(hello.Members) != 1 || hello.Members[0] != "a" {
		t.Fatalf("hello = %+v, want clientId b and members [a]", hello)
	}
	if got := recv(t, a, proto.TypeJoined); got.From != "b" {
		t.Fatalf("a saw joined from %q, want b", got.From)
	}
}

func TestFanOutStampsSeqAndFrom(t *testing.T) {
	h := startHub(t, testCfg())
	ctx := context.Background()

	clients := []*Client{NewClient("a", 16), NewClient("b", 16), NewClient("c", 16)}
	var rm *Room
	for _, c := range clients {
		r, err := h.Join(ctx, "r", c)
		if err != nil {
			t.Fatal(err)
		}
		rm = r
	}

	if err := rm.Submit(ctx, clients[1], op("op-1")); err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		got := recv(t, c, proto.TypeOp)
		if got.Seq != 1 || got.From != "b" || got.ClientOpID != "op-1" || got.Room != "r" {
			t.Fatalf("client %s got %+v", c.ID(), got)
		}
	}
}

// Every member must see every op, in the same order, with contiguous seqs,
// no matter how many senders race. This is the room actor's core promise.
func TestSeqIsTotalOrderUnderConcurrency(t *testing.T) {
	const nClients, nOps = 8, 50
	h := startHub(t, testCfg())
	ctx := context.Background()

	clients := make([]*Client, nClients)
	var rm *Room
	for i := range clients {
		clients[i] = NewClient(fmt.Sprint(i), nClients*nOps+nClients*2)
		r, err := h.Join(ctx, "r", clients[i])
		if err != nil {
			t.Fatal(err)
		}
		rm = r
	}

	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range nOps {
				if err := rm.Submit(ctx, c, op(fmt.Sprintf("%s-%d", c.ID(), i))); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	var reference []string
	for i, c := range clients {
		var order []string
		for seq := uint64(1); seq <= nClients*nOps; seq++ {
			got := recv(t, c, proto.TypeOp)
			if got.Seq != seq {
				t.Fatalf("client %s: seq %d, want %d", c.ID(), got.Seq, seq)
			}
			order = append(order, got.ClientOpID)
		}
		if i == 0 {
			reference = order
			continue
		}
		for j := range order {
			if order[j] != reference[j] {
				t.Fatalf("client %s saw %s at %d, client 0 saw %s", c.ID(), order[j], j, reference[j])
			}
		}
	}
}

func TestSlowConsumerIsDroppedNotWaitedFor(t *testing.T) {
	h := startHub(t, testCfg())
	ctx := context.Background()

	fast := NewClient("fast", 64)
	slow := NewClient("slow", 1) // holds only its hello
	rm, err := h.Join(ctx, "r", fast)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(ctx, "r", slow); err != nil {
		t.Fatal(err)
	}

	// Two ops: the first overflows slow's outbox and gets it dropped.
	for i := range 2 {
		if err := rm.Submit(ctx, fast, op(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-slow.Done():
		if slow.Reason() != ReasonSlowConsumer {
			t.Fatalf("reason = %q, want %q", slow.Reason(), ReasonSlowConsumer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow client was not dropped")
	}

	// The fast client keeps receiving and is told the slow one left.
	if got := recv(t, fast, proto.TypeLeft); got.From != "slow" {
		t.Fatalf("left from %q, want slow", got.From)
	}
	if got := recv(t, fast, proto.TypeOp); got.Seq != 2 {
		t.Fatalf("fast missed ops: seq %d", got.Seq)
	}
	if h.SlowDrops() != 1 {
		t.Fatalf("SlowDrops = %d, want 1", h.SlowDrops())
	}

	// Submitting on a dropped client fails instead of hanging.
	if err := rm.Submit(ctx, slow, op("late")); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("Submit after drop = %v, want ErrRoomClosed", err)
	}
}

func TestIdleRoomIsRetiredAndRecreated(t *testing.T) {
	cfg := testCfg()
	cfg.IdleTimeout = 20 * time.Millisecond
	h := startHub(t, cfg)
	ctx := context.Background()

	a := NewClient("a", 16)
	rm, err := h.Join(ctx, "r", a)
	if err != nil {
		t.Fatal(err)
	}
	rm.Leave(a)
	if a.Reason() != ReasonLeft {
		t.Fatalf("reason = %q, want %q", a.Reason(), ReasonLeft)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := h.RoomCount(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("room not retired, count = %d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-rm.closed:
	case <-time.After(time.Second):
		t.Fatal("retired room goroutine did not exit")
	}

	// Joining again after retirement creates a fresh room with seq reset.
	b := NewClient("b", 16)
	rm2, err := h.Join(ctx, "r", b)
	if err != nil {
		t.Fatal(err)
	}
	if rm2 == rm {
		t.Fatal("got the retired room back")
	}
	if got := recv(t, b, proto.TypeHello); got.Seq != 0 {
		t.Fatalf("fresh room seq = %d, want 0", got.Seq)
	}
}

func TestShutdownClosesEveryClient(t *testing.T) {
	h := NewHub(testCfg(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.Run(ctx)
	}()

	clients := []*Client{NewClient("a", 4), NewClient("b", 4)}
	for i, c := range clients {
		if _, err := h.Join(ctx, fmt.Sprint("room", i), c); err != nil {
			t.Fatal(err)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hub did not stop")
	}
	for _, c := range clients {
		select {
		case <-c.Done():
			if c.Reason() != ReasonShutdown {
				t.Fatalf("%s reason = %q", c.ID(), c.Reason())
			}
		default:
			t.Fatalf("client %s still open after shutdown", c.ID())
		}
	}
	if _, err := h.Join(context.Background(), "r", NewClient("late", 1)); !errors.Is(err, ErrHubClosed) {
		t.Fatalf("Join after shutdown = %v, want ErrHubClosed", err)
	}
}
