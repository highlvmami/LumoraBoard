// Package room holds the concurrency core: a Hub that owns rooms and a Room
// actor per whiteboard.
//
// Every room is a single goroutine that exclusively owns its state (members
// and the sequence counter). All interaction goes through channels, so there
// is no mutex around room state and no way for two goroutines to race on it.
package room

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

// ErrRoomClosed is returned when a client talks to a room that has exited.
var ErrRoomClosed = errors.New("room closed")

// Room is the actor for one whiteboard. Callers hold a *Room only while they
// are a member; every method is safe to call from any goroutine.
type Room struct {
	name string
	hub  *Hub
	log  *slog.Logger

	joinCh  chan joinReq  // only the hub sends here
	leaveCh chan leaveReq // members leaving
	inCh    chan inbound  // ops from members
	closed  chan struct{} // closed when the actor exits

	// State below is owned by the run goroutine.
	members map[*Client]struct{}
	seq     uint64
}

type joinReq struct {
	client *Client
	reply  chan error
}

type leaveReq struct {
	client *Client
	done   chan struct{}
}

type inbound struct {
	from *Client
	env  proto.Envelope
}

func newRoom(name string, hub *Hub) *Room {
	return &Room{
		name:    name,
		hub:     hub,
		log:     hub.log.With("room", name),
		joinCh:  make(chan joinReq, 16),
		leaveCh: make(chan leaveReq),
		inCh:    make(chan inbound, hub.cfg.InboundBuffer),
		closed:  make(chan struct{}),
		members: make(map[*Client]struct{}),
	}
}

// Name returns the room's name.
func (r *Room) Name() string { return r.name }

// Submit hands an op to the room. It blocks while the room's inbound queue
// is full, which is deliberate: that back-pressure slows a chatty client
// down instead of letting it grow the queue without bound.
func (r *Room) Submit(ctx context.Context, from *Client, env proto.Envelope) error {
	// Checked first so a dropped client gets a deterministic error even
	// while the inbound queue has room.
	select {
	case <-from.Done():
		return ErrRoomClosed
	default:
	}
	select {
	case r.inCh <- inbound{from: from, env: env}:
		return nil
	case <-from.Done():
		return ErrRoomClosed
	case <-r.closed:
		return ErrRoomClosed
	case <-ctx.Done():
		return ctx.Err()
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

	var (
		idle       = time.NewTimer(r.hub.cfg.IdleTimeout)
		retireCh   chan<- retireReq // nil until we want to retire
		retireDone = make(chan bool, 1)
	)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			r.closeAll(ReasonShutdown)
			return

		case req := <-r.joinCh:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			retireCh = nil
			r.join(req.client)
			req.reply <- nil

		case req := <-r.leaveCh:
			r.remove(req.client, ReasonLeft)
			close(req.done)
			if len(r.members) == 0 {
				idle.Reset(r.hub.cfg.IdleTimeout)
			}

		case in := <-r.inCh:
			if _, ok := r.members[in.from]; !ok {
				continue // dropped between Submit and here
			}
			r.seq++
			in.env.Seq = r.seq
			in.env.From = in.from.ID()
			in.env.Room = r.name
			r.broadcast(proto.Encode(in.env))

		case <-idle.C:
			if len(r.members) == 0 {
				retireCh = r.hub.retireCh
			}

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

func (r *Room) join(c *Client) {
	members := make([]string, 0, len(r.members))
	for m := range r.members {
		members = append(members, m.ID())
	}

	// Tell the existing members first: the newcomer learns who is here
	// from its hello and does not need to see its own arrival.
	r.broadcast(proto.Encode(proto.Envelope{
		V: proto.Version, Type: proto.TypeJoined, Room: r.name, From: c.ID(),
	}))
	r.members[c] = struct{}{}

	hello := proto.Envelope{
		V:    proto.Version,
		Type: proto.TypeHello,
		Room: r.name,
		Seq:  r.seq,
		Payload: mustJSON(proto.Hello{
			ClientID: c.ID(),
			Seq:      r.seq,
			Members:  members,
		}),
	}
	if !c.trySend(proto.Encode(hello)) {
		// A fresh outbox can only be full if the buffer is absurdly small.
		r.remove(c, ReasonSlowConsumer)
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
