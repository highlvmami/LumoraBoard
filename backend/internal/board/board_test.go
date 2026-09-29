package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
)

func mustOp(t *testing.T, raw string) Op {
	t.Helper()
	op, err := DecodeOp(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("DecodeOp(%s): %v", raw, err)
	}
	return op
}

func TestAddUpdateDeleteRoundTrip(t *testing.T) {
	s := NewState()

	if err := s.Apply(mustOp(t, `{"kind":"add","id":"r1","object":{"id":"r1","kind":"rect","x":1,"y":2,"w":10,"h":20,"color":"#f00"}}`), 1, "alice"); err != nil {
		t.Fatal(err)
	}
	o, ok := s.Get("r1")
	if !ok || o.CreatedBy != "alice" || o.Version != 1 || o.W != 10 {
		t.Fatalf("after add: %+v ok=%v", o, ok)
	}

	if err := s.Apply(mustOp(t, `{"kind":"update","id":"r1","patch":{"x":5,"text":"hi"}}`), 2, "bob"); err != nil {
		t.Fatal(err)
	}
	o, _ = s.Get("r1")
	if o.X != 5 || o.Y != 2 || o.Text != "hi" || o.Version != 2 || o.CreatedBy != "alice" {
		t.Fatalf("after update: %+v", o)
	}

	if err := s.Apply(mustOp(t, `{"kind":"reorder","id":"r1","z":7}`), 3, "bob"); err != nil {
		t.Fatal(err)
	}
	if o, _ = s.Get("r1"); o.Z != 7 {
		t.Fatalf("after reorder: z=%d", o.Z)
	}

	if err := s.Apply(mustOp(t, `{"kind":"delete","id":"r1"}`), 4, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("r1"); ok || s.Len() != 0 {
		t.Fatal("object survived delete")
	}
}

func TestAppendStreamsPointsOntoStroke(t *testing.T) {
	s := NewState()
	if err := s.Apply(mustOp(t, `{"kind":"add","id":"s1","object":{"id":"s1","kind":"stroke","points":[{"x":0,"y":0}]}}`), 1, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(mustOp(t, `{"kind":"append","id":"s1","points":[{"x":1,"y":1},{"x":2,"y":2}]}`), 2, "a"); err != nil {
		t.Fatal(err)
	}
	o, _ := s.Get("s1")
	if len(o.Points) != 3 || o.Points[2].X != 2 {
		t.Fatalf("points = %+v", o.Points)
	}

	if err := s.Apply(mustOp(t, `{"kind":"add","id":"t1","object":{"id":"t1","kind":"text","text":"x"}}`), 3, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(mustOp(t, `{"kind":"append","id":"t1","points":[{"x":1,"y":1}]}`), 4, "a"); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("append to text = %v, want ErrInvalidOp", err)
	}
}

func TestApplyRejectsWithoutMutating(t *testing.T) {
	s := NewState()
	add := mustOp(t, `{"kind":"add","id":"r1","object":{"id":"r1","kind":"rect"}}`)
	if err := s.Apply(add, 1, "a"); err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()

	cases := []string{
		`{"kind":"add","id":"r1","object":{"id":"r1","kind":"rect"}}`, // duplicate id
		`{"kind":"update","id":"nope","patch":{"x":1}}`,
		`{"kind":"delete","id":"nope"}`,
		`{"kind":"reorder","id":"nope","z":1}`,
		`{"kind":"append","id":"nope","points":[{"x":1,"y":1}]}`,
	}
	for _, raw := range cases {
		if err := s.Apply(mustOp(t, raw), 2, "a"); !errors.Is(err, ErrInvalidOp) {
			t.Errorf("%s: err = %v, want ErrInvalidOp", raw, err)
		}
	}
	after := s.Snapshot()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("state changed by rejected ops:\n%v\n%v", before, after)
	}
}

func TestDecodeOpRejectsBadShapes(t *testing.T) {
	cases := map[string]string{
		"not json":         `{`,
		"bad id":           `{"kind":"delete","id":"has space"}`,
		"unknown kind":     `{"kind":"explode","id":"a"}`,
		"add without obj":  `{"kind":"add","id":"a"}`,
		"add id mismatch":  `{"kind":"add","id":"a","object":{"id":"b","kind":"rect"}}`,
		"bad object kind":  `{"kind":"add","id":"a","object":{"id":"a","kind":"blob"}}`,
		"huge coordinate":  `{"kind":"add","id":"a","object":{"id":"a","kind":"rect","x":1e12}}`,
		"nan-ish patch":    `{"kind":"update","id":"a","patch":{"x":-1e12}}`,
		"update no patch":  `{"kind":"update","id":"a"}`,
		"reorder no z":     `{"kind":"reorder","id":"a"}`,
		"append no points": `{"kind":"append","id":"a"}`,
		"stroke too wide":  `{"kind":"add","id":"a","object":{"id":"a","kind":"stroke","strokeWidth":999}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOp(json.RawMessage(raw)); !errors.Is(err, ErrInvalidOp) {
				t.Fatalf("err = %v, want ErrInvalidOp", err)
			}
		})
	}
}

func TestBoardFullIsRejected(t *testing.T) {
	s := NewState()
	for i := range MaxObjects {
		op := Op{Kind: OpAdd, ID: fmt.Sprint("o", i), Object: &Object{ID: fmt.Sprint("o", i), Kind: KindRect}}
		if err := s.Apply(op, uint64(i+1), "a"); err != nil {
			t.Fatal(err)
		}
	}
	op := Op{Kind: OpAdd, ID: "extra", Object: &Object{ID: "extra", Kind: KindRect}}
	if err := s.Apply(op, MaxObjects+1, "a"); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("err = %v, want ErrInvalidOp", err)
	}
}

func TestSnapshotIsDeterministic(t *testing.T) {
	build := func(seed uint64) []Object {
		r := rand.New(rand.NewPCG(seed, 0))
		s := NewState()
		ids := []string{"a", "b", "c", "d"}
		r.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		for i, id := range ids {
			z := int64(0)
			if id > "b" {
				z = 1
			}
			_ = s.Apply(Op{Kind: OpAdd, ID: id, Object: &Object{ID: id, Kind: KindRect, Z: z}}, uint64(i+1), "a")
		}
		return s.Snapshot()
	}
	a, b := build(1), build(2)
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Z != b[i].Z {
			t.Fatalf("snapshot order differs at %d: %v vs %v", i, a[i].ID, b[i].ID)
		}
	}
	if a[0].Z != 0 || a[len(a)-1].Z != 1 {
		t.Fatalf("not sorted by z: %+v", a)
	}
}

// FuzzApply feeds random op sequences through the board and checks the
// invariants that hold whatever the input: no panic, rejected ops leave
// the state untouched, and Snapshot round-trips through Restore.
func FuzzApply(f *testing.F) {
	f.Add([]byte(`{"kind":"add","id":"a","object":{"id":"a","kind":"rect"}}`), uint8(3))
	f.Add([]byte(`{"kind":"append","id":"a","points":[{"x":1,"y":2}]}`), uint8(1))
	f.Fuzz(func(t *testing.T, raw []byte, n uint8) {
		s := NewState()
		_ = s.Apply(Op{Kind: OpAdd, ID: "a", Object: &Object{ID: "a", Kind: KindStroke}}, 1, "seed")
		op, err := DecodeOp(json.RawMessage(raw))
		if err != nil {
			return
		}
		for i := range int(n) {
			before := fmt.Sprint(s.Snapshot())
			if err := s.Apply(op, uint64(i+2), "fuzz"); err != nil {
				if got := fmt.Sprint(s.Snapshot()); got != before {
					t.Fatalf("rejected op changed state")
				}
			}
		}
		snap := s.Snapshot()
		restored := NewState()
		restored.Restore(snap)
		if fmt.Sprint(restored.Snapshot()) != fmt.Sprint(snap) {
			t.Fatal("Restore(Snapshot()) is not identity")
		}
	})
}
