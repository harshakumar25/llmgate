// Package ratelimit is a per-key token-bucket limiter. Each key (an API key,
// or a client IP when the gateway runs open) gets a bucket that refills at a
// steady rate up to a burst size; a request spends one token. It keeps one
// tiny struct per active key and needs no background goroutine.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter enforces a per-key request rate. The zero value is not usable; call New.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
	limited uint64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// sweepAt is the bucket count that triggers dropping idle buckets. An idle
// bucket is full again, so forgetting it changes nothing for its key.
const sweepAt = 10_000

// New builds a limiter allowing perMinute requests per key on average, with
// bursts of up to burst. burst <= 0 means perMinute.
func New(perMinute, burst int) *Limiter {
	if burst <= 0 {
		burst = perMinute
	}
	return &Limiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
}

// Decision is the outcome of one Allow call, with what a client needs for the
// X-RateLimit-* and Retry-After headers.
type Decision struct {
	OK         bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration // time until one token is available, when !OK
}

// Allow spends one token from key's bucket if it has one.
func (l *Limiter) Allow(key string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= sweepAt {
			l.sweep(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
		b.last = now
	}
	d := Decision{Limit: int(l.burst)}
	if b.tokens >= 1 {
		b.tokens--
		d.OK = true
		d.Remaining = int(b.tokens)
		return d
	}
	l.limited++
	d.RetryAfter = time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return d
}

// Limited returns how many requests have been rejected.
func (l *Limiter) Limited() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limited
}

// sweep drops buckets that have refilled completely.
func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}
