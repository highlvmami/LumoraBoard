package export

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

// Source provides a board's current objects, in paint order.
type Source interface {
	Snapshot(ctx context.Context, board string) ([]board.Object, error)
}

// Notifier pushes a message to one connection. Delivery is best effort.
type Notifier interface {
	Notify(ctx context.Context, room, client, user string, msg []byte)
}

// Config tunes the export queue.
type Config struct {
	// Workers is how many exports render at once. Rendering is CPU
	// bound, so more workers than cores only adds memory.
	Workers int
	// Queue is how many exports may wait. Beyond that Submit fails fast
	// with ErrBusy instead of piling work up.
	Queue int
	// PerOwner caps one person's unfinished exports.
	PerOwner int
	// Timeout bounds one export's run time.
	Timeout time.Duration
	// Retain is how long a finished file stays downloadable.
	Retain time.Duration
}

func (c Config) withDefaults() Config {
	if c.Workers <= 0 {
		c.Workers = min(runtime.NumCPU(), 4)
	}
	if c.Queue <= 0 {
		c.Queue = 256
	}
	if c.PerOwner <= 0 {
		c.PerOwner = 3
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Minute
	}
	if c.Retain <= 0 {
		c.Retain = 10 * time.Minute
	}
	return c
}

// State is where a job is in its life.
type State string

const (
	StateQueued   State = "queued"
	StateRunning  State = "running"
	StateDone     State = "done"
	StateFailed   State = "failed"
	StateCanceled State = "canceled"
)

func (s State) finished() bool { return s == StateDone || s == StateFailed || s == StateCanceled }

// Status is a job as clients see it, over REST and in export.progress
// messages.
type Status struct {
	ID       string `json:"id"`
	Board    string `json:"board"`
	Format   Format `json:"format"`
	State    State  `json:"state"`
	Progress int    `json:"progress"` // percent
	Error    string `json:"error,omitempty"`
	Size     int    `json:"size,omitempty"`
	// File is where to download the result once State is done.
	File string `json:"file,omitempty"`
}

// Request asks for one export.
type Request struct {
	Board  string
	Format Format
	Scale  float64 // PNG only
	// User owns the job; empty in open mode, where the job id alone
	// grants access.
	User string
	// Client is the connection that gets progress messages, if any.
	Client string
}

var (
	ErrBusy      = errors.New("export queue is full, try again shortly")
	ErrTooMany   = errors.New("you already have exports running")
	ErrNotFound  = errors.New("no such export")
	ErrBadFormat = errors.New("format must be png, pdf or json")
	ErrClosed    = errors.New("export service stopped")
)

type job struct {
	Request
	id     string
	ctx    context.Context
	cancel context.CancelFunc

	// Guarded by Manager.mu.
	state    State
	progress int
	err      string
	result   []byte
	queuedAt time.Time
	endedAt  time.Time
}

// Manager runs exports on a fixed pool of workers fed by a bounded queue.
type Manager struct {
	cfg    Config
	src    Source
	notify Notifier
	log    *slog.Logger

	queue  chan *job
	base   context.Context
	stop   context.CancelFunc
	closed atomic.Bool

	mu   sync.Mutex
	jobs map[string]*job

	waitNanos atomic.Int64 // total time jobs spent queued
	started   atomic.Int64
	failed    atomic.Int64
	rejected  atomic.Int64
}

// New creates a manager. Call Run to start its workers.
func New(cfg Config, src Source, notify Notifier, log *slog.Logger) *Manager {
	cfg = cfg.withDefaults()
	base, stop := context.WithCancel(context.Background())
	return &Manager{
		cfg: cfg, src: src, notify: notify, log: log.With("component", "export"),
		queue: make(chan *job, cfg.Queue),
		base:  base, stop: stop,
		jobs: make(map[string]*job),
	}
}

// Run works the queue until ctx is cancelled, then cancels every
// unfinished job and waits for the workers to return.
func (m *Manager) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for range m.cfg.Workers {
		wg.Go(m.work)
	}
	janitor := time.NewTicker(m.cfg.Retain / 4)
	defer janitor.Stop()
	for {
		select {
		case <-ctx.Done():
			m.closed.Store(true)
			m.stop() // cancels every job's context
			wg.Wait()
			return nil
		case now := <-janitor.C:
			m.sweep(now)
		}
	}
}

// Submit queues an export and returns its status. It never blocks.
func (m *Manager) Submit(req Request) (Status, error) {
	if !req.Format.valid() {
		return Status{}, ErrBadFormat
	}
	if m.closed.Load() {
		return Status{}, ErrClosed
	}
	ctx, cancel := context.WithTimeout(m.base, m.cfg.Timeout)
	j := &job{Request: req, id: newID(), ctx: ctx, cancel: cancel, state: StateQueued, queuedAt: time.Now()}

	m.mu.Lock()
	active := 0
	for _, o := range m.jobs {
		if o.ownerKey() == j.ownerKey() && !o.state.finished() {
			active++
		}
	}
	if active >= m.cfg.PerOwner {
		m.mu.Unlock()
		cancel()
		m.rejected.Add(1)
		return Status{}, ErrTooMany
	}
	select {
	case m.queue <- j:
	default:
		m.mu.Unlock()
		cancel()
		m.rejected.Add(1)
		return Status{}, ErrBusy
	}
	m.jobs[j.id] = j
	st := j.status()
	m.mu.Unlock()
	return st, nil
}

// ownerKey is who PerOwner counts against: the account, or the
// connection in open mode.
func (j *job) ownerKey() string {
	if j.User != "" {
		return "u:" + j.User
	}
	return "c:" + j.Client
}

// Status returns a job's state. In open mode (empty owner) any caller
// with the id may see it; otherwise only its owner.
func (m *Manager) Status(id, user string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.lookup(id, user)
	if err != nil {
		return Status{}, err
	}
	return j.status(), nil
}

// Board returns the board a job belongs to, so a caller can authorize
// against it before asking for anything else.
func (m *Manager) Board(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return "", false
	}
	return j.Board, true
}

// Result returns a finished export's file.
func (m *Manager) Result(id, user string) (name string, f Format, data []byte, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.lookup(id, user)
	if err != nil {
		return "", "", nil, err
	}
	if j.state != StateDone {
		return "", "", nil, fmt.Errorf("%w: export is %s", ErrNotFound, j.state)
	}
	return j.Board + "." + string(j.Format), j.Format, j.result, nil
}

// Cancel stops a queued or running export.
func (m *Manager) Cancel(id, user string) (Status, error) {
	m.mu.Lock()
	j, err := m.lookup(id, user)
	if err != nil {
		m.mu.Unlock()
		return Status{}, err
	}
	if !j.state.finished() {
		j.cancel()
		j.state, j.endedAt = StateCanceled, time.Now()
	}
	st := j.status()
	m.mu.Unlock()
	m.push(j, st)
	return st, nil
}

func (m *Manager) lookup(id, user string) (*job, error) {
	j, ok := m.jobs[id]
	if !ok || (j.User != "" && j.User != user) {
		return nil, ErrNotFound
	}
	return j, nil
}

func (j *job) status() Status {
	st := Status{ID: j.id, Board: j.Board, Format: j.Format, State: j.state, Progress: j.progress, Error: j.err}
	if j.state == StateDone {
		st.Size = len(j.result)
		st.File = "/api/exports/" + j.id + "/file"
	}
	return st
}

func (m *Manager) work() {
	for {
		select {
		case <-m.base.Done():
			return
		case j := <-m.queue:
			m.run(j)
		}
	}
}

func (m *Manager) run(j *job) {
	defer j.cancel()
	m.mu.Lock()
	if j.state != StateQueued { // cancelled while waiting
		m.mu.Unlock()
		return
	}
	j.state = StateRunning
	st := j.status()
	m.mu.Unlock()
	m.waitNanos.Add(int64(time.Since(j.queuedAt)))
	m.started.Add(1)
	m.push(j, st)

	data, err := m.render(j)

	m.mu.Lock()
	switch {
	case j.state == StateCanceled:
		// Cancel already reported it.
		m.mu.Unlock()
		return
	case err != nil:
		j.state, j.err = StateFailed, err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			j.err = "export took too long"
		}
		m.failed.Add(1)
		m.log.Warn("export failed", "board", j.Board, "format", j.Format, "err", err)
	default:
		j.state, j.progress, j.result = StateDone, 100, data
	}
	j.endedAt = time.Now()
	st = j.status()
	m.mu.Unlock()
	m.push(j, st)
}

func (m *Manager) render(j *job) ([]byte, error) {
	objs, err := m.src.Snapshot(j.ctx, j.Board)
	if err != nil {
		return nil, fmt.Errorf("read board: %w", err)
	}
	last := 0
	return render(j.ctx, j.Format, j.Board, objs, j.Scale, func(done, total int) {
		pct := done * 99 / max(total, 1) // 100 means the file is ready
		if pct < last+5 {
			return
		}
		last = pct
		m.mu.Lock()
		if j.state != StateRunning {
			m.mu.Unlock()
			return
		}
		j.progress = pct
		st := j.status()
		m.mu.Unlock()
		m.push(j, st)
	})
}

// push sends a status to the connection that asked for the export.
func (m *Manager) push(j *job, st Status) {
	if m.notify == nil || j.Client == "" {
		return
	}
	msg := proto.Encode(proto.Envelope{V: proto.Version, Type: proto.TypeExport, Room: j.Board, Payload: mustJSON(st)})
	m.notify.Notify(context.Background(), j.Board, j.Client, j.User, msg)
}

// sweep forgets finished jobs, and their files, once they are old.
func (m *Manager) sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, j := range m.jobs {
		if j.state.finished() && now.Sub(j.endedAt) > m.cfg.Retain {
			delete(m.jobs, id)
		}
	}
}

// Stats is a snapshot of the queue for diagnostics and tests.
type Stats struct {
	Queued, Started, Failed, Rejected int64
	AvgWait                           time.Duration
}

// Stats reports queue counters.
func (m *Manager) Stats() Stats {
	s := Stats{Queued: int64(len(m.queue)), Started: m.started.Load(), Failed: m.failed.Load(), Rejected: m.rejected.Load()}
	if s.Started > 0 {
		s.AvgWait = time.Duration(m.waitNanos.Load() / s.Started)
	}
	return s
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("export: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("export: marshal payload: " + err.Error())
	}
	return data
}
