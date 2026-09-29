// Package proto defines the wire envelope shared by every WebSocket message.
//
// The envelope is versioned and op based so that Faz 2 can add typed board
// operations, and a later CRDT experiment can ride on the same shape.
package proto

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Version is the current envelope version. Clients must send it verbatim.
const Version = 1

// Message types. Client to server types are validated in Envelope.Validate;
// server to client types are only ever produced by the room.
const (
	// TypeOp is any board operation. In Faz 1 the payload is opaque and is
	// broadcast to the room unchanged, stamped with a sequence number.
	TypeOp = "op"
	// TypeCursor is a presence update: where a member's pointer is. It is
	// ephemeral, carries no seq, is never logged and may be dropped.
	TypeCursor = "cursor"

	// Chat. chat.send goes client to server and is sequenced by the room
	// like an op; chat.message is the broadcast. chat.typing is lossy
	// presence in both directions. The latest messages come with hello.
	TypeChatSend    = "chat.send"
	TypeChatMessage = "chat.message"
	TypeChatTyping  = "chat.typing"

	// TypeExport tells the connection that asked for an export how it is
	// going. Best effort; clients can poll the REST status too.
	TypeExport = "export.progress"

	// TypeHello is sent to a client right after it joins a room.
	TypeHello = "hello"
	// TypeJoined and TypeLeft announce membership changes to the room.
	TypeJoined = "joined"
	TypeLeft   = "left"
	// TypeReject tells the sender that one of its ops was not applied.
	TypeReject = "reject"
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
//
// When Resume is true the client asked to continue from a seq the room
// still has in its log: Objects is omitted and the missed ops follow as
// ordinary op messages. Otherwise Objects is the full board at Seq.
type Hello struct {
	ClientID string          `json:"clientId"`
	Seq      uint64          `json:"seq"`
	Members  []Member        `json:"members"`
	Resume   bool            `json:"resume,omitempty"`
	Objects  json.RawMessage `json:"objects,omitempty"`
	// Role is what this client may do; viewers get their ops rejected.
	Role Role `json:"role,omitempty"`
	// Chat is the latest chat messages, sent on every join.
	Chat ChatHistory `json:"chat"`
}

// Role is what a member may do on a board.
type Role string

// Roles, from most to least privileged.
const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// CanEdit reports whether the role may change the board.
func (r Role) CanEdit() bool { return r == RoleOwner || r == RoleEditor }

// Valid reports whether r is one of the known roles.
func (r Role) Valid() bool { return r == RoleOwner || r == RoleEditor || r == RoleViewer }

// Rank orders roles so the stronger of two can be kept.
func (r Role) Rank() int {
	switch r {
	case RoleOwner:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// Member identifies someone in a room. It is the payload of TypeJoined
// and an element of Hello.Members. ID is per connection; User is the
// account behind it, empty when the server runs without sign-in.
type Member struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	User   string `json:"user,omitempty"`
	Avatar string `json:"avatar,omitempty"`
	Role   Role   `json:"role,omitempty"`
}

// Cursor is the payload of a TypeCursor message, in board coordinates.
// Hidden means the pointer left the board; X and Y are then ignored.
type Cursor struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Hidden bool    `json:"hidden,omitempty"`
}

// maxCursorCoordinate matches the board's coordinate bound.
const maxCursorCoordinate = 1e7

// DecodeCursor parses and bounds-checks a cursor payload.
func DecodeCursor(payload json.RawMessage) (Cursor, error) {
	var c Cursor
	if err := json.Unmarshal(payload, &c); err != nil {
		return Cursor{}, fmt.Errorf("%w: cursor: %w", ErrBadEnvelope, err)
	}
	if c.Hidden {
		return Cursor{Hidden: true}, nil
	}
	if !(c.X >= -maxCursorCoordinate && c.X <= maxCursorCoordinate && c.Y >= -maxCursorCoordinate && c.Y <= maxCursorCoordinate) {
		return Cursor{}, fmt.Errorf("%w: cursor out of range", ErrBadEnvelope)
	}
	return c, nil
}

// MaxChatRunes caps one chat message.
const MaxChatRunes = 1000

// ChatSend is the payload of TypeChatSend. Ref optionally points at a
// board object the message is about.
type ChatSend struct {
	Text string `json:"text"`
	Ref  string `json:"ref,omitempty"`
}

// ChatMessage is a sequenced chat message: the payload of TypeChatMessage
// and an element of ChatHistory.Messages.
type ChatMessage struct {
	ID     uint64    `json:"id"`
	From   string    `json:"from"` // connection id
	User   string    `json:"user,omitempty"`
	Name   string    `json:"name"`
	Avatar string    `json:"avatar,omitempty"`
	Text   string    `json:"text"`
	Ref    string    `json:"ref,omitempty"`
	At     time.Time `json:"at"`
}

// ChatHistory is the recent chat carried by Hello. More says whether older
// messages exist; the client pages through them over REST.
type ChatHistory struct {
	Messages []ChatMessage `json:"messages"`
	More     bool          `json:"more"`
}

// ErrBadChat wraps chat validation failures. Unlike ErrBadEnvelope it
// only earns the sender a reject, not a closed connection.
var ErrBadChat = errors.New("bad chat message")

// DecodeChat parses and checks a chat.send payload. Text is trimmed.
func DecodeChat(payload json.RawMessage) (ChatSend, error) {
	var c ChatSend
	if err := json.Unmarshal(payload, &c); err != nil {
		return ChatSend{}, fmt.Errorf("%w: %w", ErrBadChat, err)
	}
	c.Text = strings.TrimSpace(c.Text)
	switch {
	case c.Text == "":
		return ChatSend{}, fmt.Errorf("%w: empty", ErrBadChat)
	case !utf8.ValidString(c.Text) || utf8.RuneCountInString(c.Text) > MaxChatRunes:
		return ChatSend{}, fmt.Errorf("%w: at most %d characters", ErrBadChat, MaxChatRunes)
	case len(c.Ref) > 64:
		return ChatSend{}, fmt.Errorf("%w: bad ref", ErrBadChat)
	}
	return c, nil
}

// Reject is the payload of a TypeReject message.
type Reject struct {
	ClientOpID string `json:"clientOpId"`
	Reason     string `json:"reason"`
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
	switch e.Type {
	case TypeOp, TypeCursor, TypeChatSend, TypeChatTyping:
	default:
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
