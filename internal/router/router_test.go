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

// streamer is a fake that streams scripted fragments, optionally failing
// after some of them.
type streamer struct {
	fake
	parts   []string
	failAt  int // fail after this many fragments; 0 = never
	failErr error
}

func (s *streamer) ChatStream(_ context.Context, _ providers.Request, onDelta func(string) error) (*providers.Response, error) {
	s.calls.Add(1)
	text := ""
	for i, p := range s.parts {
		if s.failAt > 0 && i == s.failAt {
			return nil, s.failErr
		}
		if err := onDelta(p); err != nil {
			return nil, err
		}
		text += p
	}
	return &providers.Response{Content: text, Provider: s.name}, nil
}

func TestStreamFailsOverBeforeFirstByte(t *testing.T) {
	bad := &fake{name: "bad", reply: always(status(503))}
	good := &streamer{fake: fake{name: "good"}, parts: []string{"Hel", "lo"}}
	r, _ := newTest(config.Retry{MaxAttempts: 2}, bad, good)

	var got []string
	resp, err := r.RouteStream(context.Background(), req, func(d string) error { got = append(got, d); return nil })
	if err != nil || resp.Provider != "good" || len(got) != 2 {
		t.Fatalf("resp=%v err=%v got=%q", resp, err, got)
	}
	if bad.calls.Load() != 2 {
		t.Fatalf("bad called %d times, want 2 (retried before first byte)", bad.calls.Load())
	}
}

func TestStreamDoesNotFailOverAfterFirstByte(t *testing.T) {
	a := &streamer{fake: fake{name: "a"}, parts: []string{"Hel", "lo"}, failAt: 1, failErr: status(503)}
	b := &streamer{fake: fake{name: "b"}, parts: []string{"other"}}
	r, _ := newTest(config.Retry{MaxAttempts: 3}, a, b)

	var got []string
	_, err := r.RouteStream(context.Background(), req, func(d string) error { got = append(got, d); return nil })
	if err == nil || len(got) != 1 || got[0] != "Hel" {
		t.Fatalf("err=%v got=%q, want the stream to end after Hel", err, got)
	}
	if a.calls.Load() != 1 || b.calls.Load() != 0 {
		t.Fatalf("calls a=%d b=%d; nothing may run after output started", a.calls.Load(), b.calls.Load())
	}
}

func TestStreamFallsBackToChatForNonStreamers(t *testing.T) {
	p := &fake{name: "plain", reply: always(nil)}
	r, _ := newTest(config.Retry{MaxAttempts: 1}, p)
	var got []string
	if _, err := r.RouteStream(context.Background(), req, func(d string) error { got = append(got, d); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "ok" {
		t.Fatalf("got %q, want one fragment with the whole reply", got)
	}
}

func TestClientWriteFailureDoesNotTripBreaker(t *testing.T) {
	a := &streamer{fake: fake{name: "a"}, parts: []string{"x"}}
	r, _ := newTest(config.Retry{MaxAttempts: 3, BreakerFailures: 1}, a)
	_, err := r.RouteStream(context.Background(), req, func(string) error { return errors.New("broken pipe") })
	if err == nil || a.calls.Load() != 1 || r.Stats()[0].Open {
		t.Fatalf("err=%v calls=%d open=%v", err, a.calls.Load(), r.Stats()[0].Open)
	}
}

// modelFake records which model each call asked for.
type modelFake struct {
	name, model string
	asked       []string
	err         error
}

func (f *modelFake) Name() string { return f.name }
func (f *modelFake) Chat(_ context.Context, req providers.Request) (*providers.Response, error) {
	f.asked = append(f.asked, req.Model)
	if f.err != nil {
		return nil, f.err
	}
	m := req.Model
	if m == "" {
		m = f.model
	}
	return &providers.Response{Content: "ok", Model: m, Provider: f.name}, nil
}

func TestFailoverDoesNotSendForeignModelName(t *testing.T) {
	openai := &modelFake{name: "openai", model: "gpt-4o-mini", err: status(503)}
	ollama := &modelFake{name: "ollama", model: "llama3.2"}
	r := NewWithModels([]providers.Provider{openai, ollama}, [][]string{{"gpt-4o-mini"}, {"llama3.2"}}, config.Retry{MaxAttempts: 1}, true)
	r.sleep = func(context.Context, time.Duration) error { return nil }

	resp, err := r.Route(context.Background(), providers.Request{Model: "gpt-4o-mini", Messages: req.Messages})
	if err != nil {
		t.Fatal(err)
	}
	if openai.asked[0] != "gpt-4o-mini" {
		t.Fatalf("openai asked for %q", openai.asked[0])
	}
	if ollama.asked[0] != "" || resp.Model != "llama3.2" {
		t.Fatalf("ollama was asked for %q (resp model %q); it must use its own model", ollama.asked[0], resp.Model)
	}
}

func TestProvidersServingTheModelGoFirst(t *testing.T) {
	a := &modelFake{name: "a", model: "small"}
	b := &modelFake{name: "b", model: "big"}
	r := NewWithModels([]providers.Provider{a, b}, [][]string{{"small"}, {"big"}}, config.Retry{MaxAttempts: 1}, true)
	resp, err := r.Route(context.Background(), providers.Request{Model: "big", Messages: req.Messages})
	if err != nil || resp.Provider != "b" || len(a.asked) != 0 {
		t.Fatalf("resp=%v err=%v a.asked=%v; b serves big and should be tried first", resp, err, a.asked)
	}
}

func TestUnknownModelWithoutSubstitution(t *testing.T) {
	a := &modelFake{name: "a", model: "small"}
	r := NewWithModels([]providers.Provider{a}, [][]string{{"small"}}, config.Retry{MaxAttempts: 1}, false)
	if _, err := r.Route(context.Background(), providers.Request{Model: "nope", Messages: req.Messages}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err = %v, want ErrUnknownModel", err)
	}
	if len(a.asked) != 0 {
		t.Fatal("no provider should be called for an unknown model")
	}
}
