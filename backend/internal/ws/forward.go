package ws

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/highlvmami/lumoraboard/backend/internal/store"
)

// Cluster says which instance runs a board (see package cluster).
type Cluster interface {
	Owner(ctx context.Context, board string) (lease store.Lease, self bool, err error)
}

// WithCluster makes the handler forward clients whose board runs on
// another instance. secret signs the identity sent along, so the owner
// can trust it without seeing the user's cookie; every instance must
// share it.
func (h *Handler) WithCluster(c Cluster, secret []byte) *Handler {
	h.cluster, h.secret = c, secret
	return h
}

const (
	forwardHeader = "X-Lumora-Forward"
	// forwardMaxAge bounds replay of a captured header.
	forwardMaxAge = 30 * time.Second
	// proxyReadLimit is what a forwarding instance accepts from the
	// owner: a hello can carry a whole board.
	proxyReadLimit = 64 << 20
)

// errBadForward rejects a forwarded request whose signature does not hold.
var errBadForward = errors.New("bad forwarded identity")

type forwarded struct {
	Identity
	At int64 `json:"at"` // unix seconds
}

// signForward encodes id as payload.signature, both base64url.
func signForward(secret []byte, id Identity, now time.Time) string {
	payload, _ := json.Marshal(forwarded{Identity: id, At: now.Unix()})
	enc := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// readForward checks a forwarded identity. ok is false when the header is
// absent; err is set when it is present but not valid.
func readForward(secret []byte, r *http.Request, now time.Time) (id Identity, ok bool, err error) {
	raw := r.Header.Get(forwardHeader)
	if raw == "" {
		return Identity{}, false, nil
	}
	if len(secret) == 0 {
		return Identity{}, true, errBadForward
	}
	enc, sig, found := strings.Cut(raw, ".")
	if !found {
		return Identity{}, true, errBadForward
	}
	want := hmac.New(sha256.New, secret)
	want.Write([]byte(enc))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, want.Sum(nil)) {
		return Identity{}, true, errBadForward
	}
	payload, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return Identity{}, true, errBadForward
	}
	var f forwarded
	if err := json.Unmarshal(payload, &f); err != nil {
		return Identity{}, true, errBadForward
	}
	if age := now.Sub(time.Unix(f.At, 0)); age > forwardMaxAge || age < -forwardMaxAge {
		return Identity{}, true, errBadForward
	}
	return f.Identity, true, nil
}

// upstreamURL turns the owner's base address into its socket URL.
func upstreamURL(addr string, q url.Values) (string, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/ws"
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// proxy relays a client to the instance that owns its room. Frames go
// through unchanged in both directions; when the owner closes, the client
// gets the same close code, and when the owner vanishes it is told to
// reconnect, which lands it on whichever instance takes the board over.
func (h *Handler) proxy(ctx context.Context, client *websocket.Conn, addr string, q url.Values, id Identity) error {
	target, err := upstreamURL(addr, q)
	if err != nil {
		_ = client.Close(websocket.StatusTryAgainLater, "room unavailable")
		return err
	}
	hdr := http.Header{}
	hdr.Set(forwardHeader, signForward(h.secret, id, time.Now()))
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	up, resp, err := websocket.Dial(dctx, target, &websocket.DialOptions{HTTPHeader: hdr})
	cancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		// The owner may have just died; its lease runs out shortly and the
		// client's retry lands on the new owner.
		_ = client.Close(websocket.StatusTryAgainLater, "room unavailable")
		return err
	}
	up.SetReadLimit(proxyReadLimit)

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	type end struct {
		fromOwner bool
		err       error
	}
	ends := make(chan end, 2)
	pipe := func(from, to *websocket.Conn, fromOwner bool) {
		for {
			typ, data, err := from.Read(ctx)
			if err == nil {
				wctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
				err = to.Write(wctx, typ, data)
				cancel()
				if err != nil {
					fromOwner = !fromOwner // the write side failed
				}
			}
			if err != nil {
				ends <- end{fromOwner, err}
				return
			}
		}
	}
	go pipe(client, up, false)
	go pipe(up, client, true)

	ping := time.NewTicker(h.cfg.ReadTimeout / 2)
	defer ping.Stop()
	var first end
wait:
	for {
		select {
		case first = <-ends:
			break wait
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
			err := client.Ping(pctx)
			cancel()
			if err != nil {
				first = end{fromOwner: false, err: err}
				break wait
			}
		}
	}

	if first.fromOwner {
		code, reason := websocket.CloseStatus(first.err), ""
		var ce websocket.CloseError
		if errors.As(first.err, &ce) {
			reason = ce.Reason
		}
		if code == -1 {
			code, reason = websocket.StatusServiceRestart, "room moving"
		}
		_ = client.Close(code, reason)
		_ = up.CloseNow()
	} else {
		_ = up.Close(websocket.StatusNormalClosure, "")
		_ = client.CloseNow()
	}
	stop()
	<-ends // the other pipe
	return nil
}

// forwardParams is what the owner needs from the client's query.
func forwardParams(r *http.Request, name string, since uint64) url.Values {
	q := url.Values{"room": {name}}
	if since > 0 {
		q.Set("since", strconv.FormatUint(since, 10))
	}
	if n := r.URL.Query().Get("name"); n != "" {
		q.Set("name", n)
	}
	return q
}
