package room

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

func TestSnapshotReadsLiveRoomsAndTheStore(t *testing.T) {
	st := newTestStore()
	release := st.gateWrites() // nothing reaches the store while held
	h, _ := runHub(t, persistCfg(st))
	ctx := context.Background()

	c := NewClient("c", 16)
	rm, err := h.Join(ctx, "live", c, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := submit(ctx, rm, c, addOp("x")); err != nil {
		t.Fatal(err)
	}
	recv(t, c, proto.TypeOp)
	objs, err := h.Snapshot(ctx, "live")
	if err != nil || len(objs) != 1 || objs[0].ID != "x" {
		t.Fatalf("live snapshot = %+v, %v (the op is not in the store yet)", objs, err)
	}
	release()

	// A board with no room is read from the store, ops replayed, and no
	// room is created for it.
	if err := st.Append(ctx, "cold", []store.Record{{Seq: 1, From: "a", Op: addOp("y").Payload}}); err != nil {
		t.Fatal(err)
	}
	before, _ := h.RoomCount(ctx)
	objs, err = h.Snapshot(ctx, "cold")
	if err != nil || len(objs) != 1 || objs[0].ID != "y" {
		t.Fatalf("cold snapshot = %+v, %v", objs, err)
	}
	if after, _ := h.RoomCount(ctx); after != before {
		t.Fatalf("Snapshot created a room: %d -> %d", before, after)
	}
}

func TestNotifyReachesOnlyTheMatchingMember(t *testing.T) {
	h := startHub(t, testCfg())
	ctx := context.Background()
	a := NewClient("a", 16).WithAccount("u1", "", proto.RoleEditor)
	b := NewClient("b", 16).WithAccount("u2", "", proto.RoleEditor)
	if _, err := h.Join(ctx, "r", a, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}
	msg := proto.Encode(proto.Envelope{V: proto.Version, Type: proto.TypeExport, Room: "r"})
	h.Notify(ctx, "r", "a", "u2", msg) // wrong owner: dropped
	h.Notify(ctx, "r", "a", "u1", msg)
	h.Notify(ctx, "nope", "a", "u1", msg) // no such room: no-op
	recv(t, a, proto.TypeExport)
	// b never gets it: everything before a's cursor is something else.
	rm, _ := h.find(ctx, "r")
	rm.Cursor(a, proto.Cursor{X: 1, Y: 1})
	for {
		var env proto.Envelope
		if err := json.Unmarshal(<-b.Outbox(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Type == proto.TypeExport {
			t.Fatal("b got a's export notice")
		}
		if env.Type == proto.TypeCursor {
			return
		}
	}
}
