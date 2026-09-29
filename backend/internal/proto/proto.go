// Package proto defines the wire envelope shared by every WebSocket message.
//
// The envelope is versioned and op based so that Faz 2 can add typed board
// operations, and a later CRDT experiment can ride on the same shape.
package proto

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the current envelope version. Clients must send it verbatim.
const Version = 1

// Message types. Client to server types are validated in Envelope.Validate;
// server to client types are only ever produced by the room.
const (
	// TypeOp is any board operation. In Faz 1 the payload is opaque and is
	// broadcast to the room unchanged, stamped with a sequence number.
	TypeOp = "op"

	// TypeHello is sent to a client right after it joins a room.
	TypeHello = "hello"
	// TypeJoined and TypeLeft announce membership changes to the room.
	TypeJoined = "joined"
	TypeLeft   = "left"
	// TypeError is sent before the server closes a misbehaving connection.
	TypeError = "error"
)

// Envelope is the shape of every message on the wire.
type Envelope struct {
	V          int             `json:"v"`
	Type       string          `json:"type"`
	Room       string          `json:"room,omitempty"`
	Seq        uint64          `json:"seq,omitempty"`
	From       string          `json:"from,omitempty"`
	ClientOpID string          `json:"clientOpId,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// Hello is the payload of a TypeHello message.
type Hello struct {
	ClientID string   `json:"clientId"`
	Seq      uint64   `json:"seq"`
	Members  []string `json:"members"`
}

// ErrorPayload is the payload of a TypeError message.
type ErrorPayload struct {
	Reason string `json:"reason"`
}

// ErrBadEnvelope is wrapped by every validation failure.
var ErrBadEnvelope = errors.New("bad envelope")

// Decode parses and validates a client message.
func Decode(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrBadEnvelope, err)
	}
	if err := env.Validate(); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// Validate checks the fields a client is allowed to set.
func (e Envelope) Validate() error {
	if e.V != Version {
		return fmt.Errorf("%w: version %d, want %d", ErrBadEnvelope, e.V, Version)
	}
	if e.Type != TypeOp {
		return fmt.Errorf("%w: client may not send type %q", ErrBadEnvelope, e.Type)
	}
	if e.Seq != 0 || e.From != "" {
		return fmt.Errorf("%w: seq and from are assigned by the server", ErrBadEnvelope)
	}
	return nil
}

// Encode marshals an envelope. A marshal failure here is a programming
// error, so it panics rather than returning an error every caller ignores.
func Encode(e Envelope) []byte {
	data, err := json.Marshal(e)
	if err != nil {
		panic("proto: encode envelope: " + err.Error())
	}
	return data
}
