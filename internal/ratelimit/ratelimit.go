// Package ratelimit is an in-memory token-bucket limiter keyed by string
// (an IP address, an email, a user id). It's enough for a single-instance
// deployment, which CertsForever is by design.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter allows `burst` events at once, refilling at `per` per interval.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
	calls   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New allows bursts of `burst` and a sustained `per` events every `interval`.
func New(burst int, per int, interval time.Duration) *Limiter {
	return &Limiter{
		rate:    float64(per) / interval.Seconds(),
		burst:   float64(burst),
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
}

// Allow reports whether an event for key may proceed, and counts it if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.calls++
	if l.calls%1000 == 0 {
		l.sweep(now)
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep forgets buckets that have refilled completely (they'd be recreated
// identically), so memory stays bounded by recent activity.
func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// Len is the number of tracked keys (for tests and metrics).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
