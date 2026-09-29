package room

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

func sendChat(t *testing.T, rm *Room, c *Client, id, text string) {
	t.Helper()
	env := proto.Envelope{V: proto.Version, Type: proto.TypeChatSend, ClientOpID: id}
	if err := rm.SubmitChat(context.Background(), c, env, proto.ChatSend{Text: text}); err != nil {
		t.Fatal(err)
	}
}

func chatOf(t *testing.T, env proto.Envelope) proto.ChatMessage {
	t.Helper()
	var m proto.ChatMessage
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func rejectReason(t *testing.T, env proto.Envelope) string {
	t.Helper()
	var r proto.Reject
	if err := json.Unmarshal(env.Payload, &r); err != nil {
		t.Fatal(err)
	}
	return r.Reason
}

func TestChatIsSequencedAndFannedOut(t *testing.T) {
	h := startHub(t, testCfg())
	ctx := context.Background()
	a := NewClient("a", 16).WithName("Ayşe").WithAccount("u1", "https://img/a", proto.RoleViewer)
	b := NewClient("b", 16)
	rm, err := h.Join(ctx, "r", a, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}

	// A viewer with an account may chat even though it cannot draw.
	sendChat(t, rm, a, "m1", "merhaba")
	sendChat(t, rm, b, "m2", "selam")
	for _, c := range []*Client{a, b} {
		first := chatOf(t, recv(t, c, proto.TypeChatMessage))
		second := chatOf(t, recv(t, c, proto.TypeChatMessage))
		if first.ID != 1 || first.Name != "Ayşe" || first.User != "u1" || first.Avatar == "" || first.Text != "merhaba" || first.At.IsZero() {
			t.Fatalf("%s got %+v", c.ID(), first)
		}
		if second.ID != 2 || second.From != "b" {
			t.Fatalf("%s got %+v", c.ID(), second)
		}
	}
}

func TestGuestsCannotChat(t *testing.T) {
	h := startHub(t, testCfg())
	ctx := context.Background()
	guest := NewClient("g", 16).WithAccount("", "", proto.RoleViewer)
	other := NewClient("o", 16)
	rm, err := h.Join(ctx, "r", guest, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(ctx, "r", other, 0); err != nil {
		t.Fatal(err)
	}
	sendChat(t, rm, guest, "m1", "hi")
	rej := recv(t, guest, proto.TypeReject)
	if rej.ClientOpID != "m1" || rejectReason(t, rej) != ErrChatGuest.Error() {
		t.Fatalf("reject = %+v", rej)
	}
	// Nothing reached the other member: its next chat is the one below.
	sendChat(t, rm, other, "m2", "yo")
	if got := chatOf(t, recv(t, other, proto.TypeChatMessage)); got.ID != 1 {
		t.Fatalf("other got %+v", got)
	}
}

func TestChatRateLimitIsPerAccount(t *testing.T) {
	cfg := testCfg()
	cfg.ChatBurst, cfg.ChatRate = 2, 0.001
	h := startHub(t, cfg)
	ctx := context.Background()
	// Two tabs of the same account share one budget.
	tab1 := NewClient("t1", 16).WithAccount("u1", "", proto.RoleEditor)
	tab2 := NewClient("t2", 16).WithAccount("u1", "", proto.RoleEditor)
	someone := NewClient("s", 16).WithAccount("u2", "", proto.RoleEditor)
	rm, err := h.Join(ctx, "r", tab1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Client{tab2, someone} {
		if _, err := h.Join(ctx, "r", c, 0); err != nil {
			t.Fatal(err)
		}
	}
	sendChat(t, rm, tab1, "1", "a")
	sendChat(t, rm, tab2, "2", "b")
	sendChat(t, rm, tab2, "3", "c")
	rej := recv(t, tab2, proto.TypeReject)
	if rej.ClientOpID != "3" || rejectReason(t, rej) != ErrChatRate.Error() {
		t.Fatalf("reject = %+v", rej)
	}
	sendChat(t, rm, someone, "4", "d")
	var ids []uint64
	for range 3 {
		ids = append(ids, chatOf(t, recv(t, someone, proto.TypeChatMessage)).ID)
	}
	if fmt.Sprint(ids) != "[1 2 3]" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestChatHistoryComesWithHelloAndSurvivesRestart(t *testing.T) {
	st := newTestStore()
	ctx := context.Background()
	cfg := persistCfg(st)
	cfg.ChatBurst, cfg.ChatRate = 1000, 1000

	h, stop := runHub(t, cfg)
	a := NewClient("a", 256)
	rm, err := h.Join(ctx, "r", a, 0)
	if err != nil {
		t.Fatal(err)
	}
	total := store.ChatTail + 5
	for i := range total {
		sendChat(t, rm, a, fmt.Sprint(i), fmt.Sprint("msg ", i+1))
		recv(t, a, proto.TypeChatMessage)
	}

	// A late joiner sees the newest ChatTail messages and that more exist.
	b := NewClient("b", 16)
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}
	_, hb := helloOf(t, b)
	if n := len(hb.Chat.Messages); n != store.ChatTail || !hb.Chat.More || hb.Chat.Messages[n-1].ID != uint64(total) {
		t.Fatalf("history: %d messages, more=%v", n, hb.Chat.More)
	}
	stop()

	// After a restart the history and the id counter carry on.
	h2, _ := runHub(t, cfg)
	c := NewClient("c", 16)
	rm2, err := h2.Join(ctx, "r", c, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, hc := helloOf(t, c)
	if n := len(hc.Chat.Messages); n != store.ChatTail || hc.Chat.Messages[n-1].Text != fmt.Sprint("msg ", total) {
		t.Fatalf("restored history: %+v", hc.Chat)
	}
	sendChat(t, rm2, c, "next", "after restart")
	if got := chatOf(t, recv(t, c, proto.TypeChatMessage)); got.ID != uint64(total+1) {
		t.Fatalf("id after restart = %d", got.ID)
	}
	older, more, err := st.ChatBefore(ctx, "r", hc.Chat.Messages[0].ID, 100)
	if err != nil || more || len(older) != 5 || !strings.HasPrefix(older[0].Text, "msg 1") {
		t.Fatalf("older = %d more=%v err=%v", len(older), more, err)
	}
}

func TestTypingReachesOthersOnly(t *testing.T) {
	h := startHub(t, testCfg())
	ctx := context.Background()
	a := NewClient("a", 16)
	b := NewClient("b", 16)
	rm, err := h.Join(ctx, "r", a, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(ctx, "r", b, 0); err != nil {
		t.Fatal(err)
	}
	rm.Typing(a)
	if got := recv(t, b, proto.TypeChatTyping); got.From != "a" {
		t.Fatalf("typing from %q", got.From)
	}
	// a's own typing never comes back: everything a gets up to b's chat
	// is something else.
	sendChat(t, rm, b, "m", "x")
	for {
		var env proto.Envelope
		if err := json.Unmarshal(<-a.Outbox(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Type == proto.TypeChatTyping {
			t.Fatal("sender got its own typing notice")
		}
		if env.Type == proto.TypeChatMessage {
			return
		}
	}
}
