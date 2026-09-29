package room

import (
	"sync"
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
)

// Client is one connection's view of a room: a bounded outbox the room
// writes into and a done signal the room closes when it drops the client.
//
// The transport (see package ws) drains Outbox in its own goroutine and
// watches Done. The room never blocks on a client: if the outbox is full the
// client is dropped with ReasonSlowConsumer.
type Client struct {
	id     string
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
