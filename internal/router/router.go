// Package router runs a chat request across the provider chain. For each
// provider it retries retryable failures with jittered exponential backoff,
// fails over to the next provider when retrying cannot help, and skips
// providers whose circuit breaker is open so a dead upstream costs one fast
// check instead of a timeout per request.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/harshakumar25/llmgate/internal/config"
	"github.com/harshakumar25/llmgate/internal/providers"
)

// ErrAllOpen is returned when every provider's circuit breaker is open.
var ErrAllOpen = errors.New("all providers are cooling down after repeated failures")

// ErrUnknownModel is returned when no provider serves the requested model and
// substitution is off.
var ErrUnknownModel = errors.New("no provider serves the requested model")

// Router selects a provider and handles failover, retry and circuit breaking.
type Router struct {
	chain      []*member
	retry      config.Retry
	substitute bool // let other providers answer with their own model
	now        func() time.Time
	sleep      func(ctx context.Context, d time.Duration) error
}

// member is one provider plus its breaker state and counters.
type member struct {
	p providers.Provider

	models   map[string]bool // model names passed through as-is
	anyModel bool            // "*": pass any model name through

	mu        sync.Mutex
	failures  int       // consecutive retryable failures
	openUntil time.Time // breaker open while now < openUntil
	probing   bool      // a half-open trial call is in flight

	calls, errors, skipped uint64
	latency                time.Duration
}

// ProviderStats are cumulative per-provider counters for /metrics.
type ProviderStats struct {
	Name    string
	Calls   uint64
	Errors  uint64
	Skipped uint64 // requests that skipped this provider because its breaker was open
	Latency time.Duration
	Open    bool
}

// New builds a Router over an ordered provider chain in which every provider
// accepts any model name the client sends.
func New(chain []providers.Provider, retry config.Retry) *Router {
	models := make([][]string, len(chain))
	for i := range models {
		models[i] = []string{"*"}
	}
	return NewWithModels(chain, models, retry, true)
}

// NewWithModels builds a Router where models[i] lists the model names
// chain[i] serves ("*" for any). A request naming a model goes first to the
// providers that serve it. When substitute is true the remaining providers
// are tried next with their own default model, so failover still works
// across vendors; the response's model field says what actually answered.
func NewWithModels(chain []providers.Provider, models [][]string, retry config.Retry, substitute bool) *Router {
	if retry.MaxAttempts < 1 {
		retry.MaxAttempts = 1
	}
	if retry.MaxBackoff <= 0 {
		retry.MaxBackoff = 2 * time.Second
	}
	if retry.BreakerCooldown <= 0 {
		retry.BreakerCooldown = 30 * time.Second
	}
	ms := make([]*member, len(chain))
	for i, p := range chain {
		m := &member{p: p, models: map[string]bool{}}
		if i < len(models) {
			for _, name := range models[i] {
				if name == "*" {
					m.anyModel = true
				} else if name != "" {
					m.models[name] = true
				}
			}
		}
		ms[i] = m
	}
	return &Router{chain: ms, retry: retry, substitute: substitute, now: time.Now, sleep: sleepCtx}
}

// Route tries each available provider in order. It retries a provider only on
// retryable errors, and moves on immediately when the error says the request
// itself was rejected. It returns the first success, or a joined error.
func (r *Router) Route(ctx context.Context, req providers.Request) (*providers.Response, error) {
	plan, err := r.plan(req)
	if err != nil {
		return nil, err
	}
	return r.run(ctx, plan, func(p providers.Provider, req providers.Request) (*providers.Response, error) {
		return p.Chat(ctx, req)
	}, nil)
}

// target is one provider and the exact request it will receive.
type target struct {
	m   *member
	req providers.Request
}

// plan orders the chain for one request. Providers that serve the requested
// model come first, in chain order, and receive it unchanged. With
// substitution on, the others follow with the model field cleared, so each
// uses its own configured model instead of being sent a name it does not
// know (asking Ollama for "gpt-4o-mini" is a guaranteed 404).
func (r *Router) plan(req providers.Request) ([]target, error) {
	if req.Model == "" {
		out := make([]target, len(r.chain))
		for i, m := range r.chain {
			out[i] = target{m, req}
		}
		return out, nil
	}
	var serving, others []target
	sub := req
	sub.Model = ""
	for _, m := range r.chain {
		if m.anyModel || m.models[req.Model] {
			serving = append(serving, target{m, req})
		} else if r.substitute {
			others = append(others, target{m, sub})
		}
	}
	if len(serving)+len(others) == 0 {
		return nil, ErrUnknownModel
	}
	return append(serving, others...), nil
}

// RouteStream is Route for streamed replies. onDelta receives text as it
// arrives. Retry and failover work as in Route until the first fragment has
// been delivered; after that the client has partial output, so a failure ends
// the stream instead of silently restarting it on another provider.
// Providers that cannot stream are called normally and delivered in one piece.
func (r *Router) RouteStream(ctx context.Context, req providers.Request, onDelta func(string) error) (*providers.Response, error) {
	started := false
	deliver := func(d string) error {
		started = true
		if err := onDelta(d); err != nil {
			return &clientError{err}
		}
		return nil
	}
	plan, err := r.plan(req)
	if err != nil {
		return nil, err
	}
	return r.run(ctx, plan, func(p providers.Provider, req providers.Request) (*providers.Response, error) {
		if s, ok := p.(providers.Streamer); ok {
			return s.ChatStream(ctx, req, deliver)
		}
		resp, err := p.Chat(ctx, req)
		if err != nil {
			return nil, err
		}
		if resp.Content != "" {
			if err := deliver(resp.Content); err != nil {
				return nil, err
			}
		}
		return resp, nil
	}, func() bool { return started })
}

// clientError marks a failure to deliver output to the caller (for example a
// closed connection). It is the caller's problem, not the provider's.
type clientError struct{ err error }

func (e *clientError) Error() string { return "client: " + e.err.Error() }
func (e *clientError) Unwrap() error { return e.err }

// run is the shared failover loop. committed, when set, reports whether output
// has already reached the client, which forbids any further attempt.
func (r *Router) run(ctx context.Context, plan []target, call func(providers.Provider, providers.Request) (*providers.Response, error), committed func() bool) (*providers.Response, error) {
	var errs []error
	tried := false
	for _, t := range plan {
		m := t.m
		probe, ok := r.admit(m)
		if !ok {
			continue
		}
		tried = true
		for attempt := 1; attempt <= r.retry.MaxAttempts; attempt++ {
			if err := ctx.Err(); err != nil {
				if probe {
					r.release(m)
				}
				return nil, err
			}
			start := r.now()
			resp, err := call(m.p, t.req)
			// A caller that hung up, ran out of time or stopped reading says
			// nothing about the provider's health, so it is recorded but not
			// held against it.
			var ce *clientError
			callerGone := ctx.Err() != nil || errors.As(err, &ce)
			r.record(m, r.now().Sub(start), err, callerGone)
			if err == nil {
				return resp, nil
			}
			if callerGone {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, err
			}
			errs = append(errs, fmt.Errorf("%s attempt %d: %w", m.p.Name(), attempt, err))
			slog.Warn("provider call failed", "provider", m.p.Name(), "attempt", attempt, "err", err)
			if committed != nil && committed() {
				return nil, fmt.Errorf("stream interrupted: %w", err)
			}
			if !providers.Retryable(err) || attempt == r.retry.MaxAttempts {
				break
			}
			if err := r.sleep(ctx, r.backoff(attempt)); err != nil {
				return nil, err
			}
			if probe, ok = r.admit(m); !ok {
				break // this provider's breaker opened; fail over
			}
		}
	}
	if !tried {
		return nil, ErrAllOpen
	}
	return nil, fmt.Errorf("all providers failed: %w", errors.Join(errs...))
}

// backoff is the delay before retry number attempt+1: the base doubles each
// attempt up to the cap, and the wait is drawn from [d/2, d) so that many
// clients failing together do not retry in lockstep.
func (r *Router) backoff(attempt int) time.Duration {
	d := r.retry.Backoff << (attempt - 1)
	if d <= 0 || d > r.retry.MaxBackoff {
		d = r.retry.MaxBackoff
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half)
}

// admit reports whether a call to m may go ahead, and whether that call is the
// half-open trial. A closed breaker always admits. An open breaker admits
// nothing until its cooldown ends, then admits one trial call at a time.
func (r *Router) admit(m *member) (probe, ok bool) {
	if r.retry.BreakerFailures <= 0 {
		return false, true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failures < r.retry.BreakerFailures {
		return false, true
	}
	if r.now().Before(m.openUntil) || m.probing {
		m.skipped++
		return false, false
	}
	m.probing = true
	return true, true
}

// release gives back a half-open trial slot that was admitted but not used.
func (r *Router) release(m *member) {
	m.mu.Lock()
	m.probing = false
	m.mu.Unlock()
}

// record updates counters and breaker state after a call. Only retryable
// failures count against a provider's health: a 400 for a malformed request
// says nothing about whether the upstream is up.
func (r *Router) record(m *member, took time.Duration, err error, callerGone bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.latency += took
	m.probing = false
	switch {
	case err == nil:
		m.failures = 0
	case callerGone:
	case providers.Retryable(err):
		m.errors++
		m.failures++
		if r.retry.BreakerFailures > 0 && m.failures >= r.retry.BreakerFailures {
			m.openUntil = r.now().Add(r.retry.BreakerCooldown)
			if m.failures == r.retry.BreakerFailures {
				slog.Warn("circuit opened", "provider", m.p.Name(), "cooldown", r.retry.BreakerCooldown)
			}
		}
	default:
		m.errors++
	}
}

// Stats returns a snapshot of per-provider counters, in chain order.
func (r *Router) Stats() []ProviderStats {
	out := make([]ProviderStats, len(r.chain))
	for i, m := range r.chain {
		m.mu.Lock()
		out[i] = ProviderStats{
			Name:    m.p.Name(),
			Calls:   m.calls,
			Errors:  m.errors,
			Skipped: m.skipped,
			Latency: m.latency,
			Open:    r.retry.BreakerFailures > 0 && m.failures >= r.retry.BreakerFailures && r.now().Before(m.openUntil),
		}
		m.mu.Unlock()
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
