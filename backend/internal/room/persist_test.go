package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

// testStore wraps a Memory store with knobs a test can turn: a gate that
// holds writes, failures to inject, and a gate for loads.
type testStore struct {
	*store.Memory

	mu         sync.Mutex
	writeGate  chan struct{} // non-nil: writes wait for it to close
	loadGate   map[string]chan struct{}
	failWrites int // fail this many writes, then succeed
	failLoads  int
	appends    atomic.Int64
}

func newTestStore() *testStore {
	return &testStore{Memory: store.NewMemory(), loadGate: make(map[string]chan struct{})}
}

var errInjected = errors.New("injected store failure")

func (s *testStore) gateWrites() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := make(chan struct{})
	s.writeGate = g
	return func() {
		s.mu.Lock()
		s.writeGate = nil
		s.mu.Unlock()
		close(g)
	}
}

func (s *testStore) beforeWrite(ctx context.Context) error {
	s.mu.Lock()
	g := s.writeGate
	fail := s.failWrites > 0
	if fail {
		s.failWrites--
	}
	s.mu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail {
		return errInjected
	}
	return nil
}

func (s *testStore) Append(ctx context.Context, name string, recs []store.Record) error {
	if err := s.beforeWrite(ctx); err != nil {
		return err
	}
	s.appends.Add(1)
	return s.Memory.Append(ctx, name, recs)
}

func (s *testStore) Compact(ctx context.Context, name string, snap store.Snapshot) error {
	if err := s.beforeWrite(ctx); err != nil {
		return err
	}
	return s.Memory.Compact(ctx, name, snap)
}

func (s *testStore) Load(ctx context.Context, name string) (store.Loaded, error) {
	s.mu.Lock()
	g := s.loadGate[name]
	fail := s.failLoads > 0
	if fail {
		s.failLoads--
	}
	s.mu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return store.Loaded{}, ctx.Err()
		}
	}
	if fail {
		return store.Loaded{}, errInjected
	}
	return s.Memory.Load(ctx, name)
}

// runHub starts a hub whose lifetime the test controls; stop cancels it
// and waits for every room and persister to finish.
func runHub(t *testing.T, cfg Config) (h *Hub, stop func()) {
	t.Helper()
	h = NewHub(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.Run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return h, stop
}

func persistCfg(s store.Store) Config {
	cfg := testCfg()
	cfg.Store = s
	cfg.FlushInterval = 5 * time.Millisecond
	return cfg
}

func helloOf(t *testing.T, c *Client) (proto.Envelope, proto.Hello) {
	t.Helper()
	env := recv(t, c, proto.TypeHello)
	var h proto.Hello
	if err := json.Unmarshal(env.Payload, &h); err != nil {
		t.Fatal(err)
	}
	return env, h
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestBoardSurvivesRestart(t *testing.T) {
	st := newTestStore()
	ctx := context.Background()

	h, stop := runHub(t, persistCfg(st))
	a := NewClient("a", 64)
	rm, err := h.Join(ctx, "r", a, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := submit(ctx, rm, a, addOp(fmt.Sprint("o", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := submit(ctx, rm, a, rawOp("u", `{"kind":"update","id":"o1","patch":{"x":42}}`)); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		recv(t, a, proto.TypeOp)
	}
	stop() // shutdown flushes what is queued

	h2, _ := runHub(t, persistCfg(st))
	b := NewClient("b", 64)
	if _, err := h2.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}
	env, hello := helloOf(t, b)
	if env.Seq != 6 || hello.Resume {
		t.Fatalf("hello seq = %d resume = %v, want seq 6 and a snapshot", env.Seq, hello.Resume)
	}
	var objs []struct {
		ID string  `json:"id"`
		X  float64 `json:"x"`
	}
	if err := json.Unmarshal(hello.Objects, &objs); err != nil {
		t.Fatal(err)
	}
	if len(objs) != 5 {
		t.Fatalf("restored %d objects, want 5", len(objs))
	}
	for _, o := range objs {
		if o.ID == "o1" && o.X != 42 {
			t.Fatalf("o1.x = %v, want 42", o.X)
		}
	}

	// A client that saw seq 3 before the restart resumes from the op log
	// rebuilt from the store instead of downloading the whole board.
	c := NewClient("c", 64)
	if _, err := h2.Join(ctx, "r", c, 3); err != nil {
		t.Fatal(err)
	}
	if _, hello := helloOf(t, c); !hello.Resume {
		t.Fatal("reconnect after restart did not resume")
	}
	for want := uint64(4); want <= 6; want++ {
		if got := recv(t, c, proto.TypeOp); got.Seq != want || got.From != "a" {
			t.Fatalf("replayed %+v, want seq %d from a", got, want)
		}
	}
}

func TestSnapshotsCompactTheOpLog(t *testing.T) {
	st := newTestStore()
	cfg := persistCfg(st)
	cfg.SnapshotEvery = 5
	h, stop := runHub(t, cfg)
	ctx := context.Background()

	a := NewClient("a", 64)
	rm, _ := h.Join(ctx, "r", a, 0)
	for i := range 12 {
		if err := submit(ctx, rm, a, addOp(fmt.Sprint("o", i))); err != nil {
			t.Fatal(err)
		}
		recv(t, a, proto.TypeOp)
	}
	stop()

	loaded, err := st.Load(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Snapshot.Seq != 10 || len(loaded.Snapshot.Objects) != 10 {
		t.Fatalf("snapshot at seq %d with %d objects, want 10/10", loaded.Snapshot.Seq, len(loaded.Snapshot.Objects))
	}
	if st.OpCount("r") != 2 {
		t.Fatalf("store keeps %d ops, want the 2 after the snapshot", st.OpCount("r"))
	}
}

// The core promise of Faz 4: a slow database never slows the room's live
// traffic down beyond the queue it was given, and nothing accepted is lost.
func TestSlowStoreBackPressuresOpsButNotPresence(t *testing.T) {
	st := newTestStore()
	cfg := persistCfg(st)
	cfg.PersistQueue = 4
	cfg.BatchSize = 2 // so at most 4+2 ops are held before the room stalls
	cfg.InboundBuffer = 64
	h, _ := runHub(t, cfg)
	ctx := context.Background()

	a := NewClient("a", 256)
	rm, _ := h.Join(ctx, "r", a, 0)
	recv(t, a, proto.TypeHello)
	b := NewClient("b", 256)
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}
	recv(t, b, proto.TypeHello)

	release := st.gateWrites()
	const n = 20
	for i := range n {
		if err := submit(ctx, rm, a, addOp(fmt.Sprint("o", i))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "a persist stall", func() bool { return h.PersistStalls() > 0 })

	// The room has stopped taking ops, so fewer than n were broadcast...
	seen := 0
	for len(b.Outbox()) > 0 {
		var env proto.Envelope
		_ = json.Unmarshal(<-b.Outbox(), &env)
		if env.Type == proto.TypeOp {
			seen++
		}
	}
	if seen >= n {
		t.Fatalf("all %d ops went out while the store was stalled", seen)
	}
	// ...but it is not blocked: presence still flows.
	rm.Cursor(a, proto.Cursor{X: 1, Y: 2})
	if got := recv(t, b, proto.TypeCursor); got.From != "a" {
		t.Fatalf("cursor from %q", got.From)
	}

	release()
	for seen < n {
		recv(t, b, proto.TypeOp)
		seen++
	}
	waitFor(t, "every op to reach the store", func() bool {
		l, _ := st.Load(ctx, "r")
		return len(l.Ops) == n
	})
}

func TestFailedWritesAreRetried(t *testing.T) {
	st := newTestStore()
	st.failWrites = 3
	h, stop := runHub(t, persistCfg(st))
	ctx := context.Background()

	a := NewClient("a", 16)
	rm, _ := h.Join(ctx, "r", a, 0)
	if err := submit(ctx, rm, a, addOp("x")); err != nil {
		t.Fatal(err)
	}
	recv(t, a, proto.TypeOp)
	waitFor(t, "the op to be written", func() bool { return st.appends.Load() == 1 })
	stop()

	if h.PersistFailures() != 3 {
		t.Fatalf("PersistFailures = %d, want 3", h.PersistFailures())
	}
	if l, _ := st.Load(ctx, "r"); len(l.Ops) != 1 {
		t.Fatalf("stored ops = %d, want 1", len(l.Ops))
	}
}

func TestShutdownGivesUpOnADeadStore(t *testing.T) {
	st := newTestStore()
	st.failWrites = 1 << 30
	cfg := persistCfg(st)
	cfg.ShutdownFlush = 50 * time.Millisecond
	h, stop := runHub(t, cfg)
	ctx := context.Background()

	a := NewClient("a", 16)
	rm, _ := h.Join(ctx, "r", a, 0)
	if err := submit(ctx, rm, a, addOp("x")); err != nil {
		t.Fatal(err)
	}
	recv(t, a, proto.TypeOp)

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown hung on a store that never recovers")
	}
}

func TestLoadFailureFailsJoinThenRecovers(t *testing.T) {
	st := newTestStore()
	st.failLoads = 3 // every attempt of the first load
	h, _ := runHub(t, persistCfg(st))
	ctx := context.Background()

	if _, err := h.Join(ctx, "r", NewClient("a", 16), 0); !errors.Is(err, ErrLoadFailed) {
		t.Fatalf("Join = %v, want ErrLoadFailed", err)
	}
	b := NewClient("b", 16)
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatalf("second Join = %v", err)
	}
	recv(t, b, proto.TypeHello)
}

func TestSlowLoadDoesNotBlockOtherRooms(t *testing.T) {
	st := newTestStore()
	gate := make(chan struct{})
	st.loadGate["slow"] = gate
	h, _ := runHub(t, persistCfg(st))
	ctx := context.Background()

	slowJoined := make(chan error, 1)
	s := NewClient("s", 16)
	go func() {
		_, err := h.Join(ctx, "slow", s, 0)
		slowJoined <- err
	}()

	f := NewClient("f", 16)
	joinCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := h.Join(joinCtx, "fast", f, 0); err != nil {
		t.Fatalf("join to another room while one is loading: %v", err)
	}
	select {
	case err := <-slowJoined:
		t.Fatalf("slow room answered before its load finished: %v", err)
	default:
	}

	close(gate)
	if err := <-slowJoined; err != nil {
		t.Fatal(err)
	}
	recv(t, s, proto.TypeHello)
}

func TestIdleRetirementFlushesFirst(t *testing.T) {
	st := newTestStore()
	cfg := persistCfg(st)
	cfg.IdleTimeout = 10 * time.Millisecond
	cfg.FlushInterval = time.Hour // only the retirement flush writes
	h, _ := runHub(t, cfg)
	ctx := context.Background()

	a := NewClient("a", 16)
	rm, _ := h.Join(ctx, "r", a, 0)
	if err := submit(ctx, rm, a, addOp("x")); err != nil {
		t.Fatal(err)
	}
	recv(t, a, proto.TypeOp)
	rm.Leave(a)

	select {
	case <-rm.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("room was not retired")
	}
	// Retired rooms leave their board in the store, snapshot included.
	l, err := st.Load(ctx, "r")
	if err != nil || l.Snapshot.Seq != 1 || len(l.Snapshot.Objects) != 1 {
		t.Fatalf("after retirement store has %+v, %v", l, err)
	}

	b := NewClient("b", 16)
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}
	if env, _ := helloOf(t, b); env.Seq != 1 {
		t.Fatalf("recreated room seq = %d, want 1", env.Seq)
	}
}
