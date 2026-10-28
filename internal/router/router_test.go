package router

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harshakumar25/llmgate/internal/config"
	"github.com/harshakumar25/llmgate/internal/providers"
)

// fake is a provider whose replies are scripted per call.
type fake struct {
	name  string
	calls atomic.Int32
	reply func(n int) error // n is the 1-based call number; nil error = success
}

func (f *fake) Name() string { return f.name }

func (f *fake) Chat(context.Context, providers.Request) (*providers.Response, error) {
	n := int(f.calls.Add(1))
	if err := f.reply(n); err != nil {
		return nil, err
	}
	return &providers.Response{Content: "ok", Provider: f.name}, nil
}

func always(err error) func(int) error { return func(int) error { return err } }

func status(code int) error { return &providers.StatusError{Provider: "fake", Code: code} }

// newTest builds a router with a fake clock and instant sleeps.
func newTest(retry config.Retry, chain ...providers.Provider) (*Router, *time.Time) {
	r := New(chain, retry)
	now := time.Unix(0, 0)
	r.now = func() time.Time { return now }
	r.sleep = func(context.Context, time.Duration) error { return nil }
	return r, &now
}

var req = providers.Request{Messages: []providers.Message{{Role: "user", Content: "hi"}}}

func TestRetriesRetryableThenSucceeds(t *testing.T) {
	p := &fake{name: "a", reply: func(n int) error {
		if n == 1 {
			return status(503)
		}
		return nil
	}}
	r, _ := newTest(config.Retry{MaxAttempts: 3}, p)
	if _, err := r.Route(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if c := p.calls.Load(); c != 2 {
		t.Fatalf("calls = %d, want 2", c)
	}
}

func TestNonRetryableFailsOverWithoutRetrying(t *testing.T) {
	a := &fake{name: "a", reply: always(status(401))}
	b := &fake{name: "b", reply: always(nil)}
	r, _ := newTest(config.Retry{MaxAttempts: 3}, a, b)
	resp, err := r.Route(context.Background(), req)
	if err != nil || resp.Provider != "b" {
		t.Fatalf("resp=%v err=%v, want success from b", resp, err)
	}
	if c := a.calls.Load(); c != 1 {
		t.Fatalf("a called %d times; a 401 must not be retried", c)
	}
}

func TestBreakerOpensSkipsAndRecovers(t *testing.T) {
	healthy := atomic.Bool{}
	a := &fake{name: "a", reply: func(int) error {
		if healthy.Load() {
			return nil
		}
		return status(502)
	}}
	b := &fake{name: "b", reply: always(nil)}
	retry := config.Retry{MaxAttempts: 1, BreakerFailures: 3, BreakerCooldown: 10 * time.Second}
	r, now := newTest(retry, a, b)

	for i := 0; i < 3; i++ {
		r.Route(context.Background(), req)
	}
	if !r.Stats()[0].Open {
		t.Fatal("breaker should be open after 3 failures")
	}

	// While open, a is skipped entirely and b serves.
	for i := 0; i < 5; i++ {
		resp, err := r.Route(context.Background(), req)
		if err != nil || resp.Provider != "b" {
			t.Fatalf("resp=%v err=%v", resp, err)
		}
	}
	if c := a.calls.Load(); c != 3 {
		t.Fatalf("a called %d times while open, want 3 total", c)
	}
	if s := r.Stats()[0].Skipped; s != 5 {
		t.Fatalf("skipped = %d, want 5", s)
	}

	// After the cooldown a gets one trial; success closes the breaker.
	*now = now.Add(11 * time.Second)
	healthy.Store(true)
	resp, err := r.Route(context.Background(), req)
	if err != nil || resp.Provider != "a" {
		t.Fatalf("trial: resp=%v err=%v, want a", resp, err)
	}
	if r.Stats()[0].Open {
		t.Fatal("breaker should close after a successful trial")
	}
}

func TestFailedTrialReopensBreaker(t *testing.T) {
	a := &fake{name: "a", reply: always(status(500))}
	retry := config.Retry{MaxAttempts: 1, BreakerFailures: 2, BreakerCooldown: 10 * time.Second}
	r, now := newTest(retry, a)
	r.Route(context.Background(), req)
	r.Route(context.Background(), req)

	if _, err := r.Route(context.Background(), req); !errors.Is(err, ErrAllOpen) {
		t.Fatalf("err = %v, want ErrAllOpen", err)
	}
	*now = now.Add(11 * time.Second)
	r.Route(context.Background(), req) // trial fails
	if _, err := r.Route(context.Background(), req); !errors.Is(err, ErrAllOpen) {
		t.Fatalf("after failed trial err = %v, want ErrAllOpen", err)
	}
	if c := a.calls.Load(); c != 3 {
		t.Fatalf("calls = %d, want 3", c)
	}
}

func TestClientErrorsDoNotTripBreaker(t *testing.T) {
	a := &fake{name: "a", reply: always(status(400))}
	r, _ := newTest(config.Retry{MaxAttempts: 1, BreakerFailures: 2}, a)
	for i := 0; i < 10; i++ {
		r.Route(context.Background(), req)
	}
	if r.Stats()[0].Open {
		t.Fatal("400s opened the breaker")
	}
}

func TestCancelledCallerDoesNotTripBreaker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fake{name: "a", reply: func(int) error { cancel(); return context.Canceled }}
	r, _ := newTest(config.Retry{MaxAttempts: 3, BreakerFailures: 1}, a)
	if _, err := r.Route(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if a.calls.Load() != 1 || r.Stats()[0].Open {
		t.Fatal("a cancelled caller was retried or opened the breaker")
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	r := New(nil, config.Retry{Backoff: 100 * time.Millisecond, MaxBackoff: time.Second})
	for i := 0; i < 200; i++ {
		if d := r.backoff(1); d < 50*time.Millisecond || d >= 100*time.Millisecond {
			t.Fatalf("attempt 1 backoff %s outside [50ms,100ms)", d)
		}
		if d := r.backoff(3); d < 200*time.Millisecond || d >= 400*time.Millisecond {
			t.Fatalf("attempt 3 backoff %s outside [200ms,400ms)", d)
		}
		if d := r.backoff(40); d < 500*time.Millisecond || d >= time.Second {
			t.Fatalf("attempt 40 backoff %s outside cap", d)
		}
	}
}
