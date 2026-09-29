package ratelimit

import (
	"testing"
	"time"
)

func TestBucket(t *testing.T) {
	b := New(3, 1)
	t0 := time.Unix(1000, 0)
	for i := range 3 {
		if !b.Allow(t0) {
			t.Fatalf("burst event %d refused", i)
		}
	}
	if b.Allow(t0) {
		t.Fatal("fourth event in the same instant allowed")
	}
	if b.Allow(t0.Add(500 * time.Millisecond)) {
		t.Fatal("allowed before a whole token refilled")
	}
	if !b.Allow(t0.Add(1100 * time.Millisecond)) {
		t.Fatal("refused after refill")
	}
	// A long quiet spell refills only up to the burst.
	later := t0.Add(time.Hour)
	for range 3 {
		if !b.Allow(later) {
			t.Fatal("refused within burst after idling")
		}
	}
	if b.Allow(later) {
		t.Fatal("idle time let the bucket exceed its burst")
	}
}
