package ws

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

func TestForwardSignature(t *testing.T) {
	secret := []byte("0123456789abcdef")
	now := time.Now()
	id := Identity{User: "u1", Name: "Ayşe", Role: proto.RoleViewer}
	req := func(v string) *http.Request {
		r := &http.Request{Header: http.Header{}}
		if v != "" {
			r.Header.Set(forwardHeader, v)
		}
		return r
	}

	good := signForward(secret, id, now)
	// The payload of an owner's header joined with the viewer's signature.
	ownerPayload, _, _ := strings.Cut(signForward(secret, Identity{User: "u1", Role: proto.RoleOwner}, now), ".")
	_, goodSig, _ := strings.Cut(good, ".")
	if got, ok, err := readForward(secret, req(good), now); err != nil || !ok || got != id {
		t.Fatalf("good = %+v %v %v", got, ok, err)
	}
	if _, ok, err := readForward(secret, req(""), now); ok || err != nil {
		t.Fatalf("absent = %v %v", ok, err)
	}
	for name, c := range map[string]struct {
		secret []byte
		value  string
		at     time.Time
	}{
		"other secret":  {[]byte("another-secret!!"), good, now},
		"no secret":     {nil, good, now},
		"stale":         {secret, good, now.Add(time.Minute)},
		"tampered":      {secret, "x" + good, now},
		"no signature":  {secret, "abc", now},
		"garbage sig":   {secret, "abc.!!!", now},
		"role upgraded": {secret, ownerPayload + "." + goodSig, now},
	} {
		if _, ok, err := readForward(c.secret, req(c.value), c.at); !ok || err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestUpstreamURL(t *testing.T) {
	q := url.Values{"room": {"r"}, "since": {"3"}}
	for addr, want := range map[string]string{
		"http://10.0.0.2:8080":   "ws://10.0.0.2:8080/ws?room=r&since=3",
		"https://a.example/app/": "wss://a.example/app/ws?room=r&since=3",
	} {
		if got, err := upstreamURL(addr, q); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", addr, got, err, want)
		}
	}
}
