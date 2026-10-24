// Package router runs a chat request across the provider chain, failing over
// to the next provider and retrying with backoff on error.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/harshakumar25/llmgate/internal/config"
	"github.com/harshakumar25/llmgate/internal/providers"
)

// Router selects a provider and handles failover + retry.
type Router struct {
	chain []providers.Provider
	retry config.Retry
}

// New builds a Router over an ordered provider chain.
func New(chain []providers.Provider, retry config.Retry) *Router {
	if retry.MaxAttempts < 1 {
		retry.MaxAttempts = 1
	}
	return &Router{chain: chain, retry: retry}
}

// Route tries each provider in order. For each provider it retries up to
// MaxAttempts times with a fixed backoff before moving to the next one.
// It returns the first successful response, or a joined error if all fail.
func (r *Router) Route(ctx context.Context, req providers.Request) (*providers.Response, error) {
	var errs []error
	for _, p := range r.chain {
		for attempt := 1; attempt <= r.retry.MaxAttempts; attempt++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			resp, err := p.Chat(ctx, req)
			if err == nil {
				return resp, nil
			}
			errs = append(errs, fmt.Errorf("%s attempt %d: %w", p.Name(), attempt, err))
			slog.Warn("provider call failed", "provider", p.Name(), "attempt", attempt, "err", err)
			if attempt < r.retry.MaxAttempts {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(r.retry.Backoff):
				}
			}
		}
	}
	return nil, fmt.Errorf("all providers failed: %w", errors.Join(errs...))
}
