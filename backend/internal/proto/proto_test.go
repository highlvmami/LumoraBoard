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
