// Package room holds the concurrency core: a Hub that owns rooms and a Room
// actor per whiteboard.
//
// Every room is a single goroutine that exclusively owns its state (members,
// the board and the sequence counter). All interaction goes through channels,
// so there is no mutex around room state and no way for two goroutines to
// race on it.
package room

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/board"
	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

// ErrRoomClosed is returned when a client talks to a room that has exited.
var ErrRoomClosed = errors.New("room closed")

// Room is the actor for one whiteboard. Callers hold a *Room only while they
// are a member; every method is safe to call from any goroutine.
type Room struct {
	name string
	hub  *Hub
	log  *slog.Logger

	joinCh   chan joinReq   // only the hub sends here
	leaveCh  chan leaveReq  // members leaving
	inCh     chan inbound   // ops from members
	cursorCh chan cursorMsg // presence from members; lossy
	closed   chan struct{}  // closed when the actor exits

	// State below is owned by the run goroutine.
	members map[*Client]struct{}
	seq     uint64
	board   *board.State
	// oplog holds the most recent broadcast ops so a client that
	// reconnects with a recent seq can catch up without a full snapshot.
	oplog []logEntry

	persist   *persister
	loaded    bool            // board restored from the store
	loadCh    chan loadResult // non-nil while a load is running
	waiting   []joinReq       // joins that arrived before the board loaded
	sinceSnap int             // ops accepted since the last snapshot was queued
	stalled   bool            // not taking ops: the persist queue is full
}

type loadResult struct {
	data store.Loaded
	err  error
}

type logEntry struct {
	seq uint64
	msg []byte
}

type joinReq struct {
	client *Client
	since  uint64 // last seq the client has seen; 0 for a fresh join
	reply  chan hubJoinResult
}

type leaveReq struct {
	client *Client
	done   chan struct{}
}

type cursorMsg struct {
	from *Client
	cur  proto.Cursor
}

type inbound struct {
	from *Client
	env  proto.Envelope
	op   board.Op
}

func newRoom(name string, hub *Hub) *Room {
	return &Room{
		name:     name,
		hub:      hub,
		log:      hub.log.With("room", name),
		joinCh:   make(chan joinReq, 16),
		leaveCh:  make(chan leaveReq),
		inCh:     make(chan inbound, hub.cfg.InboundBuffer),
		cursorCh: make(chan cursorMsg, hub.cfg.InboundBuffer),
		closed:   make(chan struct{}),
		members:  make(map[*Client]struct{}),
		board:    board.NewState(),
		persist:  newPersister(name, hub),
	}
}

// Name returns the room's name.
func (r *Room) Name() string { return r.name }

// Submit hands a decoded op to the room. It blocks while the room's inbound
// queue is full, which is deliberate: that back-pressure slows a chatty
// client down instead of letting it grow the queue without bound.
//
// Decoding happens on the caller's goroutine so that the parse cost is
// spread across connections rather than serialized in the room.
func (r *Room) Submit(ctx context.Context, from *Client, env proto.Envelope, op board.Op) error {
	// Checked first so a dropped client gets a deterministic error even
	// while the inbound queue has room.
	select {
	case <-from.Done():
		return ErrRoomClosed
	default:
	}
	select {
	case r.inCh <- inbound{from: from, env: env, op: op}:
		return nil
	case <-from.Done():
		return ErrRoomClosed
	case <-r.closed:
		return ErrRoomClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cursor hands a presence update to the room without ever blocking. When
// the room is busy the update is dropped and counted: a newer position is
// never far behind, and ops must not wait behind cursor traffic.
func (r *Room) Cursor(from *Client, cur proto.Cursor) {
	select {
	case r.cursorCh <- cursorMsg{from: from, cur: cur}:
	default:
		r.hub.cursorDrops.Add(1)
	}
}

// Leave removes the client from the room. It returns once the room has
// processed the departure, or immediately if the room already dropped it.
func (r *Room) Leave(c *Client) {
	req := leaveReq{client: c, done: make(chan struct{})}
	select {
	case r.leaveCh <- req:
		<-req.done
	case <-c.Done():
	case <-r.closed:
	}
}

// run is the actor loop. It exits on ctx cancellation or after the hub
// approves retirement of an idle room.
func (r *Room) run(ctx context.Context) {
	defer close(r.closed)
	defer r.hub.wg.Done()

	var loaders sync.WaitGroup
	go r.persist.run()
	defer func() {
		// Let a running load see ctx and finish, then have the persister
		// write out what is queued before the room is gone.
		loaders.Wait()
		close(r.persist.in)
		<-r.persist.done
	}()
	r.startLoad(ctx, &loaders)

	var (
		idle       = time.NewTimer(r.hub.cfg.IdleTimeout)
		retireCh   chan<- retireReq // nil until we want to retire
		retireDone = make(chan bool, 1)
		flushed    chan struct{} // non-nil while an idle flush is running
	)
	defer idle.Stop()

	for {
		// Ops are only taken once the board is loaded and while the
		// persister has room for them; otherwise they wait in inCh and
		// Submit blocks, which slows the senders down.
		ops := r.inCh
		var wake <-chan struct{}
		if r.loaded && r.updateStall() {
			ops = nil
			wake = r.persist.wake
		} else if !r.loaded {
			ops = nil
		}

		select {
		case <-ctx.Done():
			r.closeAll(ReasonShutdown)
			r.failWaiting(ErrRoomClosed)
			close(r.persist.shutdown)
			return

		case req := <-r.joinCh:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			retireCh = nil
			flushed = nil
			if !r.loaded {
				r.waiting = append(r.waiting, req)
				if r.loadCh == nil {
					r.startLoad(ctx, &loaders) // the last attempt failed; try again
				}
				continue
			}
			r.join(req.client, req.since)
			req.reply <- hubJoinResult{room: r}

		case res := <-r.loadCh:
			r.loadCh = nil
			if res.err != nil {
				r.log.Error("could not load board", "err", res.err)
				r.failWaiting(ErrLoadFailed)
				if len(r.members) == 0 {
					idle.Reset(r.hub.cfg.IdleTimeout)
				}
				continue
			}
			r.restore(res.data)
			for _, req := range r.waiting {
				r.join(req.client, req.since)
				req.reply <- hubJoinResult{room: r}
			}
			r.waiting = nil

		case req := <-r.leaveCh:
			r.remove(req.client, ReasonLeft)
			close(req.done)
			if len(r.members) == 0 {
				idle.Reset(r.hub.cfg.IdleTimeout)
			}

		case in := <-ops:
			if _, ok := r.members[in.from]; !ok {
				continue // dropped between Submit and here
			}
			r.apply(in)

		case <-wake:
			// The persister wrote something; the loop re-checks the queue.

		case cm := <-r.cursorCh:
			if _, ok := r.members[cm.from]; !ok {
				continue
			}
			r.cursor(cm)

		case <-idle.C:
			if len(r.members) != 0 || len(r.waiting) != 0 {
				continue
			}
			if !r.loaded {
				// Nothing to save; a failed load leaves nothing behind.
				retireCh = r.hub.retireCh
				continue
			}
			// Everything must be in the store before the hub may forget
			// this room, or a new room of the same name could load a
			// stale board. Queue a snapshot (so the next load is quick)
			// and a flush, and retire once the flush is done.
			if len(r.persist.in) == cap(r.persist.in) {
				// No slot even for the flush request: the store is
				// behind. Look again shortly.
				idle.Reset(r.hub.cfg.FlushInterval)
				continue
			}
			if r.sinceSnap > 0 && !r.persist.full() {
				r.queueSnapshot()
			}
			flushed = make(chan struct{})
			r.persistSend(persistItem{flush: flushed})

		case <-flushed:
			flushed = nil
			retireCh = r.hub.retireCh

		case retireCh <- retireReq{room: r, reply: retireDone}:
			// Sending the request is only possible when the hub is ready
			// to handle it, and the hub is the only writer to joinCh, so
			// nothing can be queued for us while we wait for the verdict.
			if <-retireDone {
				return
			}
			retireCh = nil
		}
	}
}

// updateStall decides whether the room takes ops this turn. It stops when
// the persist queue is full and resumes only once it is half empty, so a
// store that is just keeping up does not flip the room on and off per op.
func (r *Room) updateStall() bool {
	switch {
	case !r.stalled && r.persist.full():
		r.stalled = true
		r.hub.persistStalls.Add(1)
		r.log.Warn("persist queue full; pausing ops until the store catches up")
	case r.stalled && len(r.persist.in) <= cap(r.persist.in)/2:
		r.stalled = false
		r.log.Info("store caught up; taking ops again")
	}
	return r.stalled
}

// ErrLoadFailed is returned by Join when the room could not load its board
// from the store. Retrying later may succeed.
var ErrLoadFailed = errors.New("room: could not load board")

// startLoad reads the board from the store on its own goroutine, with a
// few quick retries, so that a slow database delays only this room's
// joiners and never the room's other work or the hub.
func (r *Room) startLoad(ctx context.Context, wg *sync.WaitGroup) {
	ch := make(chan loadResult, 1)
	r.loadCh = ch
	wg.Add(1)
	go func() {
		defer wg.Done()
		var res loadResult
		backoff := 100 * time.Millisecond
		for attempt := 1; attempt <= 3; attempt++ {
			res.data, res.err = r.hub.cfg.Store.Load(ctx, r.name)
			if res.err == nil || ctx.Err() != nil {
				break
			}
			r.log.Warn("load failed", "attempt", attempt, "err", res.err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			backoff *= 2
		}
		ch <- res
	}()
}

// restore rebuilds the board, seq and op log from what the store had.
func (r *Room) restore(data store.Loaded) {
	r.board.Restore(data.Snapshot.Objects)
	r.seq = data.Snapshot.Seq
	for _, rec := range data.Ops {
		op, err := board.DecodeOp(rec.Op)
		if err == nil {
			err = r.board.Apply(op, rec.Seq, rec.From)
		}
		if err != nil {
			// The room only ever stored ops it had applied, so this means
			// the data was edited by hand. Skip it rather than refuse to
			// open the board.
			r.log.Error("skipping stored op", "seq", rec.Seq, "err", err)
		}
		r.seq = rec.Seq
		r.remember(rec.Seq, proto.Encode(proto.Envelope{
			V: proto.Version, Type: proto.TypeOp, Room: r.name, Seq: rec.Seq,
			From: rec.From, ClientOpID: rec.ClientOpID, Payload: rec.Op,
		}))
	}
	r.sinceSnap = len(data.Ops)
	r.loaded = true
	if r.seq > 0 {
		r.log.Info("board loaded", "seq", r.seq, "objects", r.board.Len(), "replayed", len(data.Ops))
	}
}

func (r *Room) failWaiting(err error) {
	for _, req := range r.waiting {
		req.reply <- hubJoinResult{err: err}
	}
	r.waiting = nil
}

// persistSend queues an item for the persister. Callers make sure there is
// space (see persister.full), so this never blocks.
func (r *Room) persistSend(it persistItem) {
	select {
	case r.persist.in <- it:
	default:
		// Unreachable while the invariant holds; failing loudly beats
		// silently blocking the room.
		panic("room: persist queue full on send")
	}
}

func (r *Room) queueSnapshot() {
	r.persistSend(persistItem{snap: &store.Snapshot{Seq: r.seq, Objects: r.board.Snapshot()}})
	r.sinceSnap = 0
}

// apply runs one op against the board. A rejected op goes back to the
// sender only; an accepted one gets the next seq and is fanned out.
func (r *Room) apply(in inbound) {
	seq := r.seq + 1
	if err := r.board.Apply(in.op, seq, in.from.ID()); err != nil {
		r.reject(in.from, in.env.ClientOpID, err)
		return
	}
	r.seq = seq
	in.env.Seq = seq
	in.env.From = in.from.ID()
	in.env.Room = r.name
	msg := proto.Encode(in.env)
	r.remember(seq, msg)
	r.broadcast(msg)

	r.persistSend(persistItem{rec: &store.Record{Seq: seq, From: in.env.From, ClientOpID: in.env.ClientOpID, Op: in.env.Payload}})
	r.sinceSnap++
	if r.sinceSnap >= r.hub.cfg.SnapshotEvery {
		// The slot kept free by persister.full is for this.
		r.queueSnapshot()
	}
}

// cursor fans a presence update out to everyone but its sender. It is
// best effort per recipient and never drops a member.
func (r *Room) cursor(cm cursorMsg) {
	msg := proto.Encode(proto.Envelope{
		V: proto.Version, Type: proto.TypeCursor, Room: r.name, From: cm.from.ID(), Payload: mustJSON(cm.cur),
	})
	for c := range r.members {
		if c != cm.from && !c.trySendLossy(msg) {
			r.hub.cursorDrops.Add(1)
		}
	}
}

func (r *Room) reject(c *Client, clientOpID string, err error) {
	c.trySend(proto.Encode(proto.Envelope{
		V:          proto.Version,
		Type:       proto.TypeReject,
		Room:       r.name,
		ClientOpID: clientOpID,
		Payload:    mustJSON(proto.Reject{ClientOpID: clientOpID, Reason: err.Error()}),
	}))
}

// remember appends msg to the op log, dropping the oldest entries once
// the log is full.
func (r *Room) remember(seq uint64, msg []byte) {
	if r.hub.cfg.OpLogSize <= 0 {
		return
	}
	if len(r.oplog) == r.hub.cfg.OpLogSize {
		copy(r.oplog, r.oplog[1:])
		r.oplog = r.oplog[:len(r.oplog)-1]
	}
	r.oplog = append(r.oplog, logEntry{seq: seq, msg: msg})
}

// canResume reports whether every op after since is still in the log.
func (r *Room) canResume(since uint64) bool {
	if since == 0 || since > r.seq {
		return false
	}
	if since == r.seq {
		return true
	}
	return len(r.oplog) > 0 && r.oplog[0].seq <= since+1
}

func (r *Room) join(c *Client, since uint64) {
	members := make([]proto.Member, 0, len(r.members))
	for m := range r.members {
		members = append(members, m.member())
	}

	// Tell the existing members first: the newcomer learns who is here
	// from its hello and does not need to see its own arrival.
	r.broadcast(proto.Encode(proto.Envelope{
		V: proto.Version, Type: proto.TypeJoined, Room: r.name, From: c.ID(), Payload: mustJSON(c.member()),
	}))
	r.members[c] = struct{}{}

	hello := proto.Hello{ClientID: c.ID(), Seq: r.seq, Members: members}
	resume := r.canResume(since)
	if resume {
		hello.Resume = true
	} else {
		hello.Objects = mustJSON(r.board.Snapshot())
	}
	env := proto.Envelope{V: proto.Version, Type: proto.TypeHello, Room: r.name, Seq: r.seq, Payload: mustJSON(hello)}
	if !c.trySend(proto.Encode(env)) {
		// A fresh outbox can only be full if the buffer is absurdly small.
		r.remove(c, ReasonSlowConsumer)
		return
	}
	if !resume {
		return
	}
	for _, e := range r.oplog {
		if e.seq <= since {
			continue
		}
		if !c.trySend(e.msg) {
			r.remove(c, ReasonSlowConsumer)
			return
		}
	}
}

// remove drops c from the room and tells everyone else.
func (r *Room) remove(c *Client, reason CloseReason) {
	if _, ok := r.members[c]; !ok {
		return
	}
	delete(r.members, c)
	c.close(reason)
	if reason == ReasonSlowConsumer {
		r.hub.slowDrops.Add(1)
		r.log.Warn("dropped slow consumer", "client", c.ID())
	}
	r.broadcast(proto.Encode(proto.Envelope{
		V: proto.Version, Type: proto.TypeLeft, Room: r.name, From: c.ID(),
	}))
}

// broadcast fans msg out to every member without ever blocking. Members
// whose outbox is full are dropped; their "left" notice goes to the rest.
func (r *Room) broadcast(msg []byte) {
	var slow []*Client
	for c := range r.members {
		if !c.trySend(msg) {
			slow = append(slow, c)
		}
	}
	for _, c := range slow {
		r.remove(c, ReasonSlowConsumer)
	}
}

func (r *Room) closeAll(reason CloseReason) {
	for c := range r.members {
		delete(r.members, c)
		c.close(reason)
	}
}
