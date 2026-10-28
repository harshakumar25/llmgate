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

// Router selects a provider and handles failover, retry and circuit breaking.
type Router struct {
	chain []*member
	retry config.Retry
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// member is one provider plus its breaker state and counters.
type member struct {
	p providers.Provider

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

// New builds a Router over an ordered provider chain.
func New(chain []providers.Provider, retry config.Retry) *Router {
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
		ms[i] = &member{p: p}
	}
	return &Router{chain: ms, retry: retry, now: time.Now, sleep: sleepCtx}
}

// Route tries each available provider in order. It retries a provider only on
// retryable errors, and moves on immediately when the error says the request
// itself was rejected. It returns the first success, or a joined error.
func (r *Router) Route(ctx context.Context, req providers.Request) (*providers.Response, error) {
	var errs []error
	tried := false
	for _, m := range r.chain {
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
			resp, err := m.p.Chat(ctx, req)
			// A caller that hung up or ran out of time says nothing about the
			// provider's health, so it is recorded but not held against it.
			callerGone := ctx.Err() != nil
			r.record(m, r.now().Sub(start), err, callerGone)
			if err == nil {
				return resp, nil
			}
			if callerGone {
				return nil, ctx.Err()
			}
			errs = append(errs, fmt.Errorf("%s attempt %d: %w", m.p.Name(), attempt, err))
			slog.Warn("provider call failed", "provider", m.p.Name(), "attempt", attempt, "err", err)
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
