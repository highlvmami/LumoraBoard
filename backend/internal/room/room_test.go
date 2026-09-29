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

	"github.com/highlvmami/lumoraboard/backend/internal/board"
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
	return Config{InboundBuffer: 8, IdleTimeout: time.Hour, OpLogSize: 100}
}

// addOp builds an envelope that adds a rect whose object id is the op id.
func addOp(id string) proto.Envelope {
	payload := fmt.Sprintf(`{"kind":"add","id":%q,"object":{"id":%q,"kind":"rect","x":1,"y":1}}`, id, id)
	return proto.Envelope{V: proto.Version, Type: proto.TypeOp, ClientOpID: id, Payload: json.RawMessage(payload)}
}

// rawOp builds an op envelope from a payload literal.
func rawOp(clientOpID, payload string) proto.Envelope {
	return proto.Envelope{V: proto.Version, Type: proto.TypeOp, ClientOpID: clientOpID, Payload: json.RawMessage(payload)}
}

// submit decodes the envelope's payload the way the transport does, then
// hands it to the room.
func submit(ctx context.Context, rm *Room, c *Client, env proto.Envelope) error {
	op, err := board.DecodeOp(env.Payload)
	if err != nil {
		return err
	}
	return rm.Submit(ctx, c, env, op)
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
	if _, err := h.Join(ctx, "r", a, 0); err != nil {
		t.Fatal(err)
	}
	b := NewClient("b", 16)
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}

	var hello proto.Hello
	if err := json.Unmarshal(recv(t, b, proto.TypeHello).Payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.ClientID != "b" || len(hello.Members) != 1 || hello.Members[0].ID != "a" {
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
		r, err := h.Join(ctx, "r", c, 0)
		if err != nil {
			t.Fatal(err)
		}
		rm = r
	}

	if err := submit(ctx, rm, clients[1], addOp("op-1")); err != nil {
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
		r, err := h.Join(ctx, "r", clients[i], 0)
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
				if err := submit(ctx, rm, c, addOp(fmt.Sprintf("c%s-%d", c.ID(), i))); err != nil {
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
	rm, err := h.Join(ctx, "r", fast, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(ctx, "r", slow, 0); err != nil {
		t.Fatal(err)
	}

	// Two ops: the first overflows slow's outbox and gets it dropped.
	for i := range 2 {
		if err := submit(ctx, rm, fast, addOp(fmt.Sprint("o", i))); err != nil {
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
	if err := submit(ctx, rm, slow, addOp("late")); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("Submit after drop = %v, want ErrRoomClosed", err)
	}
}

func TestIdleRoomIsRetiredAndRecreated(t *testing.T) {
	cfg := testCfg()
	cfg.IdleTimeout = 20 * time.Millisecond
	h := startHub(t, cfg)
	ctx := context.Background()

	a := NewClient("a", 16)
	rm, err := h.Join(ctx, "r", a, 0)
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
	rm2, err := h.Join(ctx, "r", b, 0)
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
		if _, err := h.Join(ctx, fmt.Sprint("room", i), c, 0); err != nil {
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
	if _, err := h.Join(context.Background(), "r", NewClient("late", 1), 0); !errors.Is(err, ErrHubClosed) {
		t.Fatalf("Join after shutdown = %v, want ErrHubClosed", err)
	}
}

func hello(t *testing.T, c *Client) proto.Hello {
	t.Helper()
	var h proto.Hello
	if err := json.Unmarshal(recv(t, c, proto.TypeHello).Payload, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func objects(t *testing.T, raw json.RawMessage) []board.Object {
	t.Helper()
	var objs []board.Object
	if err := json.Unmarshal(raw, &objs); err != nil {
		t.Fatal(err)
	}
	return objs
}

func TestRejectGoesToSenderOnly(t *testing.T) {
	h := startHub(t, testCfg())
	ctx := context.Background()

	a := NewClient("a", 16)
	b := NewClient("b", 16)
	rm, _ := h.Join(ctx, "r", a, 0)
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}

	if err := submit(ctx, rm, a, rawOp("bad", `{"kind":"delete","id":"missing"}`)); err != nil {
		t.Fatal(err)
	}
	if err := submit(ctx, rm, a, addOp("ok")); err != nil {
		t.Fatal(err)
	}

	rej := recv(t, a, proto.TypeReject)
	var payload proto.Reject
	if err := json.Unmarshal(rej.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ClientOpID != "bad" || payload.Reason == "" {
		t.Fatalf("reject = %+v", payload)
	}
	// The rejected op consumed no seq: the next accepted op is seq 1.
	if got := recv(t, a, proto.TypeOp); got.Seq != 1 {
		t.Fatalf("seq after reject = %d, want 1", got.Seq)
	}
	// b only ever sees the accepted op.
	if got := recv(t, b, proto.TypeOp); got.ClientOpID != "ok" {
		t.Fatalf("b got %+v", got)
	}
	select {
	case raw := <-b.Outbox():
		t.Fatalf("b received an extra message: %s", raw)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLateJoinerGetsSnapshot(t *testing.T) {
	h := startHub(t, testCfg())
	ctx := context.Background()

	a := NewClient("a", 16)
	rm, _ := h.Join(ctx, "r", a, 0)
	if objs := objects(t, hello(t, a).Objects); len(objs) != 0 {
		t.Fatalf("fresh room snapshot has %d objects", len(objs))
	}
	for _, id := range []string{"r1", "r2"} {
		if err := submit(ctx, rm, a, addOp(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := submit(ctx, rm, a, rawOp("u", `{"kind":"update","id":"r1","patch":{"x":42}}`)); err != nil {
		t.Fatal(err)
	}
	recv(t, a, proto.TypeOp)
	recv(t, a, proto.TypeOp)
	recv(t, a, proto.TypeOp)

	b := NewClient("b", 16)
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}
	hb := hello(t, b)
	objs := objects(t, hb.Objects)
	if hb.Seq != 3 || hb.Resume || len(objs) != 2 {
		t.Fatalf("hello = seq %d resume %v objects %d", hb.Seq, hb.Resume, len(objs))
	}
	if objs[0].ID != "r1" || objs[0].X != 42 || objs[0].Version != 3 || objs[0].CreatedBy != "a" {
		t.Fatalf("snapshot object = %+v", objs[0])
	}
}

func TestReconnectResumesFromSeq(t *testing.T) {
	cfg := testCfg()
	cfg.OpLogSize = 3
	h := startHub(t, cfg)
	ctx := context.Background()

	a := NewClient("a", 64)
	rm, _ := h.Join(ctx, "r", a, 0)
	hello(t, a)
	for i := range 5 {
		if err := submit(ctx, rm, a, addOp(fmt.Sprint("o", i))); err != nil {
			t.Fatal(err)
		}
		recv(t, a, proto.TypeOp)
	}

	// Seen up to 3, missed 4 and 5: both are still in a log of size 3.
	b := NewClient("b", 16)
	if _, err := h.Join(ctx, "r", b, 3); err != nil {
		t.Fatal(err)
	}
	hb := hello(t, b)
	if !hb.Resume || hb.Objects != nil || hb.Seq != 5 {
		t.Fatalf("resume hello = %+v", hb)
	}
	for _, want := range []uint64{4, 5} {
		if got := recv(t, b, proto.TypeOp); got.Seq != want {
			t.Fatalf("replayed seq %d, want %d", got.Seq, want)
		}
	}

	// Seen only up to 1: op 2 fell out of the log, so a snapshot it is.
	c := NewClient("c", 16)
	if _, err := h.Join(ctx, "r", c, 1); err != nil {
		t.Fatal(err)
	}
	hc := hello(t, c)
	if hc.Resume || len(objects(t, hc.Objects)) != 5 {
		t.Fatalf("stale hello = resume %v objects %s", hc.Resume, hc.Objects)
	}

	// Caught up exactly: resume with nothing to replay.
	d := NewClient("d", 16)
	if _, err := h.Join(ctx, "r", d, 5); err != nil {
		t.Fatal(err)
	}
	if hd := hello(t, d); !hd.Resume {
		t.Fatalf("exact hello = %+v", hd)
	}

	// Claims a seq from the future (a retired room restarted at 0): snapshot.
	e := NewClient("e", 16)
	if _, err := h.Join(ctx, "r", e, 99); err != nil {
		t.Fatal(err)
	}
	if he := hello(t, e); he.Resume {
		t.Fatalf("future hello = %+v", he)
	}
}

// Two clients hammer the same object concurrently. Whatever interleaving
// the room picks, a third client joining afterwards must see exactly the
// state that replaying the broadcast stream produces on either client.
func TestConcurrentUpdatesConverge(t *testing.T) {
	const nOps = 100
	h := startHub(t, testCfg())
	ctx := context.Background()

	a := NewClient("a", 4*nOps)
	b := NewClient("b", 4*nOps)
	rm, _ := h.Join(ctx, "r", a, 0)
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}
	if err := submit(ctx, rm, a, addOp("shared")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, c := range []*Client{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range nOps {
				patch := fmt.Sprintf(`{"kind":"update","id":"shared","patch":{"x":%d,"text":%q}}`, i, c.ID())
				if err := submit(ctx, rm, c, rawOp(fmt.Sprintf("%s-%d", c.ID(), i), patch)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	replay := func(c *Client) board.Object {
		st := board.NewState()
		for seq := uint64(1); seq <= 2*nOps+1; seq++ {
			env := recv(t, c, proto.TypeOp)
			op, err := board.DecodeOp(env.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Apply(op, env.Seq, env.From); err != nil {
				t.Fatal(err)
			}
		}
		o, _ := st.Get("shared")
		return o
	}
	fromA, fromB := replay(a), replay(b)

	late := NewClient("late", 16)
	if _, err := h.Join(ctx, "r", late, 0); err != nil {
		t.Fatal(err)
	}
	server := objects(t, hello(t, late).Objects)[0]

	if fmt.Sprint(fromA) != fmt.Sprint(fromB) || fmt.Sprint(fromA) != fmt.Sprint(server) {
		t.Fatalf("diverged:\n a: %+v\n b: %+v\n server: %+v", fromA, fromB, server)
	}
	if server.Version != 2*nOps+1 {
		t.Fatalf("last writer version = %d, want %d", server.Version, 2*nOps+1)
	}
}

// Presence is lossy: a recipient whose outbox is half full misses cursor
// updates but is never dropped for them, and the sender never gets its own.
func TestCursorIsLossyAndNeverDropsMembers(t *testing.T) {
	h := startHub(t, testCfg())
	ctx := context.Background()

	a := NewClient("a", 16)
	rm, err := h.Join(ctx, "r", a, 0)
	if err != nil {
		t.Fatal(err)
	}
	recv(t, a, proto.TypeHello)
	b := NewClient("b", 4) // never drained after this
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}
	recv(t, a, proto.TypeJoined)

	rm.Cursor(b, proto.Cursor{X: 7, Y: 8})
	got := recv(t, a, proto.TypeCursor)
	if got.From != "b" {
		t.Fatalf("a got cursor from %q, want b", got.From)
	}
	// Flood from a. Updates the room queue cannot take are dropped at
	// Cursor; the ones it takes are dropped per recipient once b's
	// outbox is half full.
	for i := range 50 {
		rm.Cursor(a, proto.Cursor{X: float64(i)})
	}
	// A round trip through the room guarantees the queued cursors ran.
	if err := submit(ctx, rm, a, addOp("sync")); err != nil {
		t.Fatal(err)
	}
	recv(t, a, proto.TypeOp)

	// b's outbox took cursors only while at most half full, so the op
	// still fit and b is still a member.
	if h.CursorDrops() == 0 {
		t.Fatal("no cursor drops recorded")
	}
	select {
	case <-b.Done():
		t.Fatalf("b was dropped (%s) for presence traffic", b.Reason())
	default:
	}

}
