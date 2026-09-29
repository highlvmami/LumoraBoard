package proto

import (
	"errors"
	"testing"
)

func TestDecodeAcceptsClientOp(t *testing.T) {
	env, err := Decode([]byte(`{"v":1,"type":"op","clientOpId":"c1","payload":{"kind":"stroke"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if env.ClientOpID != "c1" || string(env.Payload) != `{"kind":"stroke"}` {
		t.Fatalf("decoded %+v", env)
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := map[string]string{
		"not json":         `{`,
		"wrong version":    `{"v":2,"type":"op"}`,
		"server only type": `{"v":1,"type":"hello"}`,
		"client sets seq":  `{"v":1,"type":"op","seq":5}`,
		"client sets from": `{"v":1,"type":"op","from":"x"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(raw))
			if !errors.Is(err, ErrBadEnvelope) {
				t.Fatalf("err = %v, want ErrBadEnvelope", err)
			}
		})
	}
}

func TestDecodeCursor(t *testing.T) {
	if _, err := Decode([]byte(`{"v":1,"type":"cursor","payload":{"x":1,"y":2}}`)); err != nil {
		t.Fatalf("cursor envelope rejected: %v", err)
	}
	c, err := DecodeCursor([]byte(`{"x":1.5,"y":-2}`))
	if err != nil || c.X != 1.5 || c.Y != -2 || c.Hidden {
		t.Fatalf("DecodeCursor = %+v, %v", c, err)
	}
	c, err = DecodeCursor([]byte(`{"hidden":true,"x":1e99}`))
	if err != nil || !c.Hidden || c.X != 0 {
		t.Fatalf("hidden cursor = %+v, %v", c, err)
	}
	for _, raw := range []string{`{"x":1e8,"y":0}`, `{"x":0,"y":-1e8}`, `[1,2]`} {
		if _, err := DecodeCursor([]byte(raw)); !errors.Is(err, ErrBadEnvelope) {
			t.Fatalf("DecodeCursor(%s) err = %v, want ErrBadEnvelope", raw, err)
		}
	}
}
