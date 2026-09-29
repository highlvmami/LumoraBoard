// Package board is the whiteboard state and the operations that change it.
//
// The room applies ops in the order it assigns sequence numbers, so this
// package is deliberately single-threaded: last writer (highest seq) wins
// per object, which is the LWW model chosen for v1.
package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// Object kinds.
const (
	KindStroke  = "stroke"
	KindRect    = "rect"
	KindEllipse = "ellipse"
	KindArrow   = "arrow"
	KindText    = "text"
	KindSticky  = "sticky"
)

// Op kinds.
const (
	OpAdd     = "add"
	OpUpdate  = "update"
	OpDelete  = "delete"
	OpReorder = "reorder"
	OpAppend  = "append" // stream points onto a stroke while it is drawn
)

// Limits keep one client from growing a board without bound.
const (
	MaxObjects     = 10_000
	MaxPoints      = 10_000
	MaxTextLen     = 4_000
	MaxIDLen       = 64
	MaxColorLen    = 32
	MaxAppendPerOp = 512
	maxCoordinate  = 1e7
	maxStrokeWidth = 200
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Point is one vertex of a stroke or arrow.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Object is one item on the board. Pointer fields in Patch mirror these.
type Object struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	W           float64 `json:"w,omitempty"`
	H           float64 `json:"h,omitempty"`
	Points      []Point `json:"points,omitempty"`
	Text        string  `json:"text,omitempty"`
	Color       string  `json:"color,omitempty"`
	StrokeWidth float64 `json:"strokeWidth,omitempty"`
	Z           int64   `json:"z"`
	CreatedBy   string  `json:"createdBy,omitempty"`
	// Version is the seq of the last op that touched this object.
	Version uint64 `json:"version"`
}

// Patch is the partial update carried by OpUpdate. Nil means unchanged.
type Patch struct {
	X           *float64 `json:"x,omitempty"`
	Y           *float64 `json:"y,omitempty"`
	W           *float64 `json:"w,omitempty"`
	H           *float64 `json:"h,omitempty"`
	Points      []Point  `json:"points,omitempty"`
	Text        *string  `json:"text,omitempty"`
	Color       *string  `json:"color,omitempty"`
	StrokeWidth *float64 `json:"strokeWidth,omitempty"`
}

// Op is the payload of a proto.TypeOp envelope.
type Op struct {
	Kind   string  `json:"kind"`
	ID     string  `json:"id"`
	Object *Object `json:"object,omitempty"` // add
	Patch  *Patch  `json:"patch,omitempty"`  // update
	Points []Point `json:"points,omitempty"` // append
	Z      *int64  `json:"z,omitempty"`      // reorder
}

// ErrInvalidOp wraps every validation failure; the room turns it into a
// reject message for the sender instead of closing the connection.
var ErrInvalidOp = errors.New("invalid op")

// State is the whiteboard. It is not safe for concurrent use; the room
// goroutine is its only user.
type State struct {
	objects map[string]*Object
}

// NewState creates an empty board.
func NewState() *State {
	return &State{objects: make(map[string]*Object)}
}

// Len returns the number of objects on the board.
func (s *State) Len() int { return len(s.objects) }

// Get returns a copy of one object.
func (s *State) Get(id string) (Object, bool) {
	o, ok := s.objects[id]
	if !ok {
		return Object{}, false
	}
	return cloneObject(o), true
}

// Snapshot returns every object sorted by z, then id, so that two boards
// with the same content produce the same snapshot.
func (s *State) Snapshot() []Object {
	out := make([]Object, 0, len(s.objects))
	for _, o := range s.objects {
		out = append(out, cloneObject(o))
	}
	slices.SortFunc(out, func(a, b Object) int {
		if a.Z != b.Z {
			if a.Z < b.Z {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}

// Restore replaces the board content with objs. Used by tests and, in
// Faz 4, by the persister when a room is loaded from a snapshot.
func (s *State) Restore(objs []Object) {
	s.objects = make(map[string]*Object, len(objs))
	for i := range objs {
		o := cloneObject(&objs[i])
		s.objects[o.ID] = &o
	}
}

// DecodeOp parses an op payload and checks its shape. Applying it may
// still fail (unknown id, duplicate id), which Apply reports.
func DecodeOp(payload json.RawMessage) (Op, error) {
	var op Op
	if err := json.Unmarshal(payload, &op); err != nil {
		return Op{}, fmt.Errorf("%w: %w", ErrInvalidOp, err)
	}
	if err := op.validate(); err != nil {
		return Op{}, err
	}
	return op, nil
}

func (op Op) validate() error {
	if !idRe.MatchString(op.ID) {
		return fmt.Errorf("%w: id must match %s", ErrInvalidOp, idRe)
	}
	switch op.Kind {
	case OpAdd:
		if op.Object == nil {
			return fmt.Errorf("%w: add needs object", ErrInvalidOp)
		}
		if op.Object.ID != op.ID {
			return fmt.Errorf("%w: object id %q differs from op id %q", ErrInvalidOp, op.Object.ID, op.ID)
		}
		return validateObject(op.Object)
	case OpUpdate:
		if op.Patch == nil {
			return fmt.Errorf("%w: update needs patch", ErrInvalidOp)
		}
		return validatePatch(op.Patch)
	case OpDelete:
		return nil
	case OpReorder:
		if op.Z == nil {
			return fmt.Errorf("%w: reorder needs z", ErrInvalidOp)
		}
		return nil
	case OpAppend:
		if len(op.Points) == 0 || len(op.Points) > MaxAppendPerOp {
			return fmt.Errorf("%w: append needs 1..%d points", ErrInvalidOp, MaxAppendPerOp)
		}
		return validatePoints(op.Points)
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidOp, op.Kind)
	}
}

func validateObject(o *Object) error {
	switch o.Kind {
	case KindStroke, KindRect, KindEllipse, KindArrow, KindText, KindSticky:
	default:
		return fmt.Errorf("%w: unknown object kind %q", ErrInvalidOp, o.Kind)
	}
	if err := validateCoords(o.X, o.Y, o.W, o.H); err != nil {
		return err
	}
	if len(o.Points) > MaxPoints {
		return fmt.Errorf("%w: too many points", ErrInvalidOp)
	}
	if err := validatePoints(o.Points); err != nil {
		return err
	}
	if len(o.Text) > MaxTextLen || len(o.Color) > MaxColorLen {
		return fmt.Errorf("%w: text or color too long", ErrInvalidOp)
	}
	if o.StrokeWidth < 0 || o.StrokeWidth > maxStrokeWidth {
		return fmt.Errorf("%w: strokeWidth out of range", ErrInvalidOp)
	}
	return nil
}

func validatePatch(p *Patch) error {
	if p.X != nil && !finite(*p.X) || p.Y != nil && !finite(*p.Y) ||
		p.W != nil && !finite(*p.W) || p.H != nil && !finite(*p.H) {
		return fmt.Errorf("%w: coordinate out of range", ErrInvalidOp)
	}
	if len(p.Points) > MaxPoints {
		return fmt.Errorf("%w: too many points", ErrInvalidOp)
	}
	if err := validatePoints(p.Points); err != nil {
		return err
	}
	if p.Text != nil && len(*p.Text) > MaxTextLen || p.Color != nil && len(*p.Color) > MaxColorLen {
		return fmt.Errorf("%w: text or color too long", ErrInvalidOp)
	}
	if p.StrokeWidth != nil && (*p.StrokeWidth < 0 || *p.StrokeWidth > maxStrokeWidth) {
		return fmt.Errorf("%w: strokeWidth out of range", ErrInvalidOp)
	}
	return nil
}

func validateCoords(vals ...float64) error {
	for _, v := range vals {
		if !finite(v) {
			return fmt.Errorf("%w: coordinate out of range", ErrInvalidOp)
		}
	}
	return nil
}

func validatePoints(pts []Point) error {
	for _, p := range pts {
		if !finite(p.X) || !finite(p.Y) {
			return fmt.Errorf("%w: point out of range", ErrInvalidOp)
		}
	}
	return nil
}

func finite(v float64) bool { return v >= -maxCoordinate && v <= maxCoordinate }

// Apply mutates the board with an op that was assigned seq by the room
// and sent by client from. It returns ErrInvalidOp when the op does not
// fit the current state; the board is unchanged in that case.
func (s *State) Apply(op Op, seq uint64, from string) error {
	switch op.Kind {
	case OpAdd:
		if _, exists := s.objects[op.ID]; exists {
			return fmt.Errorf("%w: object %q already exists", ErrInvalidOp, op.ID)
		}
		if len(s.objects) >= MaxObjects {
			return fmt.Errorf("%w: board is full", ErrInvalidOp)
		}
		o := cloneObject(op.Object)
		o.CreatedBy = from
		o.Version = seq
		s.objects[op.ID] = &o
		return nil

	case OpUpdate:
		o, err := s.existing(op.ID)
		if err != nil {
			return err
		}
		p := op.Patch
		if p.X != nil {
			o.X = *p.X
		}
		if p.Y != nil {
			o.Y = *p.Y
		}
		if p.W != nil {
			o.W = *p.W
		}
		if p.H != nil {
			o.H = *p.H
		}
		if p.Points != nil {
			o.Points = slices.Clone(p.Points)
		}
		if p.Text != nil {
			o.Text = *p.Text
		}
		if p.Color != nil {
			o.Color = *p.Color
		}
		if p.StrokeWidth != nil {
			o.StrokeWidth = *p.StrokeWidth
		}
		o.Version = seq
		return nil

	case OpDelete:
		if _, err := s.existing(op.ID); err != nil {
			return err
		}
		delete(s.objects, op.ID)
		return nil

	case OpReorder:
		o, err := s.existing(op.ID)
		if err != nil {
			return err
		}
		o.Z = *op.Z
		o.Version = seq
		return nil

	case OpAppend:
		o, err := s.existing(op.ID)
		if err != nil {
			return err
		}
		if o.Kind != KindStroke && o.Kind != KindArrow {
			return fmt.Errorf("%w: cannot append points to a %s", ErrInvalidOp, o.Kind)
		}
		if len(o.Points)+len(op.Points) > MaxPoints {
			return fmt.Errorf("%w: too many points", ErrInvalidOp)
		}
		o.Points = append(o.Points, op.Points...)
		o.Version = seq
		return nil
	}
	return fmt.Errorf("%w: unknown kind %q", ErrInvalidOp, op.Kind)
}

func (s *State) existing(id string) (*Object, error) {
	o, ok := s.objects[id]
	if !ok {
		return nil, fmt.Errorf("%w: no object %q", ErrInvalidOp, id)
	}
	return o, nil
}

func cloneObject(o *Object) Object {
	c := *o
	c.Points = slices.Clone(o.Points)
	return c
}
