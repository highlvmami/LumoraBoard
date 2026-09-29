package ws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

func TestChatOverSocket(t *testing.T) {
	f := newFixture(t, DefaultConfig())
	a := dial(t, f, "r1")
	b := dial(t, f, "r1")
	read(t, a, proto.TypeHello)
	read(t, b, proto.TypeHello)

	send(t, a, `{"v":1,"type":"chat.send","clientOpId":"c1","payload":{"text":"  hello board  ","ref":"obj1"}}`)
	env := read(t, b, proto.TypeChatMessage)
	var m proto.ChatMessage
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != 1 || m.Text != "hello board" || m.Ref != "obj1" {
		t.Fatalf("message = %+v", m)
	}
	// The sender gets its own message back, tagged with its clientOpId.
	if echo := read(t, a, proto.TypeChatMessage); echo.ClientOpID != "c1" {
		t.Fatalf("echo = %+v", echo)
	}
}

func TestBadChatIsRejectedNotDisconnected(t *testing.T) {
	f := newFixture(t, DefaultConfig())
	a := dial(t, f, "r1")
	read(t, a, proto.TypeHello)

	send(t, a, `{"v":1,"type":"chat.send","clientOpId":"e","payload":{"text":"   "}}`)
	send(t, a, `{"v":1,"type":"chat.send","clientOpId":"l","payload":{"text":"`+strings.Repeat("ş", proto.MaxChatRunes+1)+`"}}`)
	for _, id := range []string{"e", "l"} {
		if rej := read(t, a, proto.TypeReject); rej.ClientOpID != id {
			t.Fatalf("reject for %q, want %q", rej.ClientOpID, id)
		}
	}
	send(t, a, `{"v":1,"type":"chat.send","clientOpId":"ok","payload":{"text":"still here"}}`)
	read(t, a, proto.TypeChatMessage)
}

func TestTypingIsThrottledAndSkipsSender(t *testing.T) {
	f := newFixture(t, DefaultConfig())
	a := dial(t, f, "r1")
	b := dial(t, f, "r1")
	read(t, a, proto.TypeHello)
	read(t, b, proto.TypeHello)

	for range 20 {
		send(t, a, `{"v":1,"type":"chat.typing"}`)
	}
	send(t, a, `{"v":1,"type":"chat.send","clientOpId":"x","payload":{"text":"done"}}`)

	typing := 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, data, err := b.Read(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var env proto.Envelope
		_ = json.Unmarshal(data, &env)
		if env.Type == proto.TypeChatTyping {
			typing++
		}
		if env.Type == proto.TypeChatMessage {
			break
		}
	}
	if typing != 1 {
		t.Fatalf("b saw %d typing notices, want 1", typing)
	}
}

func chatServer(t *testing.T, st store.Store, a Authorizer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	NewChatHistory(st, a, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func getChat(t *testing.T, srv *httptest.Server, path string) (int, proto.ChatHistory) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var h proto.ChatHistory
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, h
}

func TestChatHistoryPages(t *testing.T) {
	st := store.NewMemory()
	var msgs []proto.ChatMessage
	for i := uint64(1); i <= 7; i++ {
		msgs = append(msgs, proto.ChatMessage{ID: i, From: "a", Name: "A", Text: "m", At: time.Now()})
	}
	if err := st.AppendChat(context.Background(), "b1", msgs); err != nil {
		t.Fatal(err)
	}
	srv := chatServer(t, st, nil)

	code, h := getChat(t, srv, "/api/boards/b1/chat?before=6&limit=3")
	if code != http.StatusOK || len(h.Messages) != 3 || h.Messages[0].ID != 3 || h.Messages[2].ID != 5 || !h.More {
		t.Fatalf("page: %d %+v", code, h)
	}
	code, h = getChat(t, srv, "/api/boards/b1/chat?before=3")
	if code != http.StatusOK || len(h.Messages) != 2 || h.More {
		t.Fatalf("last page: %d %+v", code, h)
	}
	if code, h = getChat(t, srv, "/api/boards/empty/chat"); code != http.StatusOK || h.Messages == nil {
		t.Fatalf("empty board: %d %+v", code, h)
	}
	for _, bad := range []string{"/api/boards/b1/chat?before=x", "/api/boards/b1/chat?limit=0", "/api/boards/b%20d/chat"} {
		if code, _ := getChat(t, srv, bad); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", bad, code)
		}
	}
}

func TestChatHistoryIsAuthorized(t *testing.T) {
	for want, err := range map[int]error{http.StatusUnauthorized: ErrUnauthorized, http.StatusForbidden: ErrForbidden} {
		srv := chatServer(t, store.NewMemory(), fakeAuth{err: err})
		if code, _ := getChat(t, srv, "/api/boards/b1/chat"); code != want {
			t.Fatalf("%v: %d, want %d", err, code, want)
		}
	}
}
