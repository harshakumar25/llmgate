package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func newTest(perMinute, burst int) (*Limiter, *time.Time) {
	l := New(perMinute, burst)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	return l, &now
}

func TestBurstThenRefill(t *testing.T) {
	l, now := newTest(60, 3) // one token per second, bursts of 3
	for i := 0; i < 3; i++ {
		if d := l.Allow("k"); !d.OK || d.Remaining != 2-i {
			t.Fatalf("request %d: %+v", i, d)
		}
	}
	d := l.Allow("k")
	if d.OK || d.RetryAfter != time.Second {
		t.Fatalf("4th request: %+v, want rejected with 1s retry", d)
	}
	*now = now.Add(time.Second)
	if !l.Allow("k").OK {
		t.Fatal("token should have refilled after 1s")
	}
	if l.Limited() != 1 {
		t.Fatalf("limited = %d", l.Limited())
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l, _ := newTest(60, 1)
	if !l.Allow("a").OK || l.Allow("a").OK {
		t.Fatal("a: first allowed, second rejected")
	}
	if !l.Allow("b").OK {
		t.Fatal("b must not share a's bucket")
	}
}

func TestRefillCapsAtBurst(t *testing.T) {
	l, now := newTest(60, 2)
	l.Allow("k")
	*now = now.Add(time.Hour)
	if d := l.Allow("k"); d.Remaining != 1 {
		t.Fatalf("remaining = %d, want burst-1 after a long idle", d.Remaining)
	}
}

func TestSweepDropsOnlyIdleBuckets(t *testing.T) {
	l, now := newTest(60, 1)
	for i := 0; i < sweepAt; i++ {
		l.Allow(fmt.Sprint(i))
	}
	l.Allow("busy") // a fresh bucket that is empty after this call...
	*now = now.Add(10 * time.Second)
	l.Allow("busy") // ...refilled by now, so it is spent again
	l.Allow("trigger")
	if n := len(l.buckets); n > 3 {
		t.Fatalf("buckets = %d after sweep, want idle ones gone", n)
	}
}
