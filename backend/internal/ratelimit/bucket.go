// Package ratelimit is a token bucket small enough to keep one per
// connection or per user without thinking about it.
package ratelimit

import "time"

// Bucket allows bursts of up to Burst events, refilled at Rate per
// second. It is not safe for concurrent use: each owner (a connection's
// read pump, a room goroutine) keeps its own.
type Bucket struct {
	burst  float64
	rate   float64
	tokens float64
	last   time.Time
}

// New returns a full bucket.
func New(burst int, perSecond float64) *Bucket {
	return &Bucket{burst: float64(burst), rate: perSecond, tokens: float64(burst)}
}

// Allow takes a token if one is available at time now.
func (b *Bucket) Allow(now time.Time) bool {
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
