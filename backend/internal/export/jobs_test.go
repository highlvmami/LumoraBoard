package export

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

	"go.uber.org/goleak"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// gatedSource hands out sample boards, holding every read until the gate
// opens, and records how many reads overlap.
type gatedSource struct {
	gate    chan struct{}
	running atomic.Int32
	peak    atomic.Int32
	reads   atomic.Int32
}

func newGated() *gatedSource { return &gatedSource{gate: make(chan struct{})} }

func (s *gatedSource) Snapshot(ctx context.Context, _ string) ([]board.Object, error) {
	n := s.running.Add(1)
	defer s.running.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	s.reads.Add(1)
	select {
	case <-s.gate:
		return sample(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type openSource struct{}

func (openSource) Snapshot(context.Context, string) ([]board.Object, error) { return sample(), nil }

type recorder struct {
	mu   sync.Mutex
	msgs []Status
}

func (r *recorder) Notify(_ context.Context, _, client, _ string, msg []byte) {
	var env proto.Envelope
	_ = json.Unmarshal(msg, &env)
	var st Status
	_ = json.Unmarshal(env.Payload, &st)
	if env.Type != proto.TypeExport || client == "" {
		panic("bad notify")
	}
	r.mu.Lock()
	r.msgs = append(r.msgs, st)
	r.mu.Unlock()
}

func (r *recorder) states(id string) []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []State
	for _, m := range r.msgs {
		if m.ID == id && (len(out) == 0 || out[len(out)-1] != m.State) {
			out = append(out, m.State)
		}
	}
	return out
}

func start(t *testing.T, cfg Config, src Source, n Notifier) *Manager {
	t.Helper()
	m := New(cfg, src, n, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return m
}

func waitState(t *testing.T, m *Manager, id, user string, want State) Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err := m.Status(id, user)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s is %s, want %s", id, st.State, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestQueueIsBoundedAndWorkersCapConcurrency(t *testing.T) {
	src := newGated()
	m := start(t, Config{Workers: 2, Queue: 3, PerOwner: 100}, src, nil)

	var ids []string
	for i := range 5 {
		st, err := m.Submit(Request{Board: "b", Format: FormatJSON, User: fmt.Sprint("u", i)})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		ids = append(ids, st.ID)
		if i == 1 {
			// Let the two workers pick the first jobs up so the queue
			// holds exactly the next three.
			for src.running.Load() < 2 {
				time.Sleep(time.Millisecond)
			}
		}
	}
	if _, err := m.Submit(Request{Board: "b", Format: FormatJSON, User: "late"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("6th submit = %v, want ErrBusy", err)
	}
	close(src.gate)
	for i, id := range ids {
		waitState(t, m, id, fmt.Sprint("u", i), StateDone)
	}
	if p := src.peak.Load(); p > 2 {
		t.Fatalf("%d exports ran at once with 2 workers", p)
	}
	if s := m.Stats(); s.Rejected != 1 || s.Started != 5 {
		t.Fatalf("stats %+v", s)
	}
}

func TestHundredConcurrentRequestsAllFinish(t *testing.T) {
	src := newGated()
	close(src.gate)
	m := start(t, Config{Workers: 4, Queue: 100, PerOwner: 1}, src, nil)
	var wg sync.WaitGroup
	ids := make([]string, 100)
	for i := range ids {
		wg.Go(func() {
			st, err := m.Submit(Request{Board: "b", Format: FormatJSON, User: fmt.Sprint("u", i)})
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = st.ID
		})
	}
	wg.Wait()
	for i, id := range ids {
		waitState(t, m, id, fmt.Sprint("u", i), StateDone)
	}
	if p := src.peak.Load(); p > 4 {
		t.Fatalf("peak concurrency %d > 4 workers", p)
	}
}

func TestPerOwnerLimit(t *testing.T) {
	src := newGated()
	m := start(t, Config{Workers: 1, PerOwner: 2}, src, nil)
	for range 2 {
		if _, err := m.Submit(Request{Board: "b", Format: FormatJSON, User: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Submit(Request{Board: "b", Format: FormatJSON, User: "u"}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("3rd = %v", err)
	}
	if _, err := m.Submit(Request{Board: "b", Format: FormatJSON, User: "other"}); err != nil {
		t.Fatalf("other user: %v", err)
	}
	if _, err := m.Submit(Request{Board: "b", Format: "gif", User: "x"}); !errors.Is(err, ErrBadFormat) {
		t.Fatalf("gif = %v", err)
	}
	close(src.gate)
}

func TestCancelQueuedAndRunning(t *testing.T) {
	src := newGated()
	rec := &recorder{}
	m := start(t, Config{Workers: 1}, src, rec)
	running, _ := m.Submit(Request{Board: "b", Format: FormatPDF, User: "u", Client: "c1"})
	waitState(t, m, running.ID, "u", StateRunning)
	queued, _ := m.Submit(Request{Board: "b", Format: FormatPDF, User: "u", Client: "c1"})

	if _, err := m.Cancel(queued.ID, "someone else"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel by stranger = %v", err)
	}
	for _, id := range []string{queued.ID, running.ID} {
		if st, err := m.Cancel(id, "u"); err != nil || st.State != StateCanceled {
			t.Fatalf("cancel %s = %+v %v", id, st, err)
		}
	}
	// The running job's read saw its context end; the queued one never ran.
	waitFor(t, func() bool { return src.running.Load() == 0 })
	time.Sleep(20 * time.Millisecond)
	if n := src.reads.Load(); n != 1 {
		t.Fatalf("%d reads, want 1", n)
	}
	if _, _, _, err := m.Result(running.ID, "u"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("result of cancelled job: %v", err)
	}
	if got := fmt.Sprint(rec.states(running.ID)); got != "[running canceled]" {
		t.Fatalf("running job messages %s", got)
	}
}

func TestProgressIsPushedAndResultIsPrivate(t *testing.T) {
	rec := &recorder{}
	m := start(t, Config{}, openSource{}, rec)
	st, err := m.Submit(Request{Board: "b", Format: FormatPNG, Scale: 1, User: "u", Client: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitState(t, m, st.ID, "u", StateDone)
	if done.File == "" || done.Size == 0 || done.Progress != 100 {
		t.Fatalf("done = %+v", done)
	}
	waitFor(t, func() bool { s := rec.states(st.ID); return len(s) > 0 && s[len(s)-1] == StateDone })
	if got := fmt.Sprint(rec.states(st.ID)); got != "[running done]" {
		t.Fatalf("states %s", got)
	}
	name, f, data, err := m.Result(st.ID, "u")
	if err != nil || name != "b.png" || f != FormatPNG || len(data) != done.Size {
		t.Fatalf("result %q %q %d %v", name, f, len(data), err)
	}
	if _, err := m.Status(st.ID, "intruder"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger sees job: %v", err)
	}
}

func TestTimeoutFailsTheJob(t *testing.T) {
	src := newGated()
	m := start(t, Config{Timeout: 20 * time.Millisecond}, src, nil)
	st, _ := m.Submit(Request{Board: "b", Format: FormatJSON, User: "u"})
	if got := waitState(t, m, st.ID, "u", StateFailed); got.Error != "export took too long" {
		t.Fatalf("error %q", got.Error)
	}
	close(src.gate)
}

func TestFinishedJobsAreForgotten(t *testing.T) {
	m := start(t, Config{Retain: time.Minute}, openSource{}, nil)
	st, _ := m.Submit(Request{Board: "b", Format: FormatJSON})
	waitState(t, m, st.ID, "", StateDone)
	m.sweep(time.Now().Add(2 * time.Minute))
	if _, err := m.Status(st.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("still there: %v", err)
	}
}

func TestShutdownCancelsBlockedJobs(t *testing.T) {
	src := newGated() // never opens
	m := New(Config{Workers: 2}, src, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Run(ctx)
	}()
	for i := range 4 {
		if _, err := m.Submit(Request{Board: "b", Format: FormatPNG, User: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return src.running.Load() == 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
	if _, err := m.Submit(Request{Board: "b", Format: FormatPNG}); !errors.Is(err, ErrClosed) {
		t.Fatalf("submit after stop = %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
