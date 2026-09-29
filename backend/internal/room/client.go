package room

import (
	"sync"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

// CloseReason says why a room disconnected a client.
type CloseReason string

const (
	// ReasonSlowConsumer means the client's outbox filled up: it was not
	// reading fast enough and was dropped so the room would not block.
	ReasonSlowConsumer CloseReason = "slow consumer"
	// ReasonShutdown means the server is stopping.
	ReasonShutdown CloseReason = "server shutdown"
	// ReasonLeft means the client asked to leave.
	ReasonLeft CloseReason = "left"
	// ReasonMoved means this server no longer owns the room; reconnecting
	// reaches the new owner.
	ReasonMoved CloseReason = "room moved"
)

// Client is one connection's view of a room: a bounded outbox the room
// writes into and a done signal the room closes when it drops the client.
//
// The transport (see package ws) drains Outbox in its own goroutine and
// watches Done. The room never blocks on a client: if the outbox is full the
// client is dropped with ReasonSlowConsumer.
type Client struct {
	id     string
	name   string
	user   string
	avatar string
	role   proto.Role
	outbox chan []byte

	once   sync.Once
	done   chan struct{}
	reason CloseReason
}

// NewClient creates a client with an outbox holding up to buffer messages.
func NewClient(id string, buffer int) *Client {
	if buffer < 1 {
		buffer = 1
	}
	return &Client{
		id:     id,
		outbox: make(chan []byte, buffer),
		done:   make(chan struct{}),
	}
}

// WithName sets the display name other members see. Call it before Join;
// the room reads it only after the join hands the client over.
func (c *Client) WithName(name string) *Client {
	c.name = name
	return c
}

// WithAccount sets the signed-in account behind the connection and its
// role on the board. Like WithName, call it before Join. An empty role
// means editor, which is what the server grants when sign-in is off.
func (c *Client) WithAccount(user, avatar string, role proto.Role) *Client {
	c.user, c.avatar, c.role = user, avatar, role
	return c
}

// Name returns the client's display name, possibly empty.
func (c *Client) Name() string { return c.name }

// Role returns what the client may do on the board.
func (c *Client) Role() proto.Role {
	if c.role == "" {
		return proto.RoleEditor
	}
	return c.role
}

// member describes the client on the wire.
func (c *Client) member() proto.Member {
	return proto.Member{ID: c.id, Name: c.name, User: c.user, Avatar: c.avatar, Role: c.Role()}
}

// ID returns the client's identifier.
func (c *Client) ID() string { return c.id }

// Outbox delivers messages the room wants sent to this client.
func (c *Client) Outbox() <-chan []byte { return c.outbox }

// Done is closed once the room has dropped this client. After that no more
// messages arrive on Outbox and Reason says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Reason returns why the client was closed. Only meaningful after Done.
func (c *Client) Reason() CloseReason {
	select {
	case <-c.done:
		return c.reason
	default:
		return ""
	}
}

// close marks the client dropped. Safe to call more than once; the first
// reason wins. Only the room goroutine calls this.
func (c *Client) close(reason CloseReason) {
	c.once.Do(func() {
		c.reason = reason
		close(c.done)
	})
}

// Deliver queues a message for this client from outside the room, such as
// a reject the transport produces while decoding. It never blocks; a false
// result means the outbox is full and the room will drop the client on its
// next broadcast anyway.
func (c *Client) Deliver(msg []byte) bool { return c.trySend(msg) }

// trySendLossy queues an ephemeral message only while the outbox is at
// most half full. Presence must never eat the headroom that ops need, or
// a burst of cursor moves could get a healthy client dropped as slow.
func (c *Client) trySendLossy(msg []byte) bool {
	if len(c.outbox) > cap(c.outbox)/2 {
		return false
	}
	return c.trySend(msg)
}

// trySend queues msg without blocking. It reports false when the outbox is
// full, which is the room's cue to drop the client.
func (c *Client) trySend(msg []byte) bool {
	select {
	case c.outbox <- msg:
		return true
	default:
		return false
	}
}
