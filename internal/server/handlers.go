package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/harshakumar25/llmgate/internal/cache"
	"github.com/harshakumar25/llmgate/internal/metering"
	"github.com/harshakumar25/llmgate/internal/providers"
	"github.com/harshakumar25/llmgate/internal/router"
)

// maxBodyBytes caps a chat request body. Large enough for long multi-turn
// prompts, small enough that one client cannot exhaust memory.
const maxBodyBytes = 1 << 20

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req providers.Request
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body exceeds %d bytes", maxBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages must not be empty")
		return
	}

	// 1. Guardrails: block injection, redact PII in place.
	if res := s.deps.Guardrails.Apply(&req); res.Blocked {
		writeError(w, http.StatusForbidden, res.Reason)
		return
	}

	// 2. Cache: return a prior identical completion if fresh. A client can
	// force a fresh call with Cache-Control: no-cache.
	key := cache.Key(req)
	useCache := s.cacheTTL > 0 && !strings.Contains(r.Header.Get("Cache-Control"), "no-cache")
	if useCache {
		if cached, ok := s.deps.Cache.Get(key); ok {
			if req.Stream {
				s.streamCached(w, cached)
				return
			}
			w.Header().Set("X-LLMGate-Cache", "hit")
			writeJSON(w, http.StatusOK, cached)
			return
		}
	}

	if req.Stream {
		s.streamChat(w, r, req, key, useCache)
		return
	}

	// 3. Route across providers, coalescing identical requests already in
	// flight into one upstream call. The shared call runs detached from any
	// single client's context, so one caller hanging up does not fail the
	// others; the upstream timeout still bounds it.
	upstream := func() (*providers.Response, error) {
		resp, err := s.deps.Router.Route(context.WithoutCancel(r.Context()), req)
		if err != nil {
			return nil, err
		}
		// 4. Meter once per upstream call, not once per waiting client.
		s.deps.Meter.Record(metering.Record{
			Time:     time.Now(),
			APIKey:   bearerToken(r),
			Provider: resp.Provider,
			Model:    resp.Model,
			Usage:    resp.Usage,
			CostUSD:  metering.Cost(resp.Model, resp.Usage),
		})
		resp = s.redactOutput(resp)
		if useCache {
			s.deps.Cache.Set(key, resp, s.cacheTTL)
		}
		return resp, nil
	}
	var (
		resp   *providers.Response
		err    error
		shared bool
	)
	if useCache {
		resp, err, shared = s.flight.Do(key, upstream)
	} else {
		resp, err = upstream()
	}
	if err != nil {
		s.writeRouteError(w, err)
		return
	}

	switch {
	case shared:
		w.Header().Set("X-LLMGate-Cache", "coalesced")
	case useCache:
		w.Header().Set("X-LLMGate-Cache", "miss")
	default:
		w.Header().Set("X-LLMGate-Cache", "bypass")
	}
	writeJSON(w, http.StatusOK, resp)
}

// redactOutput returns resp with PII scrubbed from its content when output
// redaction is on. It copies rather than mutates, because resp may be shared
// with coalesced callers.
func (s *Server) redactOutput(resp *providers.Response) *providers.Response {
	text, n := s.deps.Guardrails.RedactOutput(resp.Content)
	if n == 0 {
		return resp
	}
	out := *resp
	out.Content = text
	return &out
}

// handleMetrics emits Prometheus text exposition. Replace the hand-rolled
// counters with the official client_golang collectors when you add the
// dependency.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	tokens, cost, calls := s.deps.Meter.Totals()
	cs := s.deps.Cache.Stats()
	leaders, shared := s.flight.Stats()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	metric := func(name, typ, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, v)
	}
	metric("llmgate_calls_total", "counter", "Upstream chat calls that succeeded.", calls)
	metric("llmgate_tokens_total", "counter", "Total tokens across all calls.", tokens)
	metric("llmgate_cost_usd_total", "counter", "Estimated spend in USD.", fmt.Sprintf("%f", cost))
	metric("llmgate_cache_hits_total", "counter", "Requests served from the cache.", cs.Hits)
	metric("llmgate_cache_misses_total", "counter", "Cache lookups that missed.", cs.Misses)
	metric("llmgate_cache_evictions_total", "counter", "Entries dropped to stay under the size bound.", cs.Evictions)
	metric("llmgate_cache_entries", "gauge", "Entries in the local cache tier.", cs.Entries)
	metric("llmgate_cache_remote_hits_total", "counter", "Local misses answered by Redis.", cs.RemoteHits)
	metric("llmgate_cache_remote_errors_total", "counter", "Redis calls that failed and were treated as misses.", cs.RemoteErrors)
	var limited uint64
	if s.limiter != nil {
		limited = s.limiter.Limited()
	}
	metric("llmgate_rate_limited_total", "counter", "Requests rejected with 429 by the per-key rate limit.", limited)
	metric("llmgate_coalesced_total", "counter", "Requests that shared an identical in-flight upstream call.", shared)
	metric("llmgate_upstream_flights_total", "counter", "Coalescing groups that made an upstream call.", leaders)

	ps := s.deps.Router.Stats()
	series := func(name, typ, help string, v func(router.ProviderStats) any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, p := range ps {
			fmt.Fprintf(w, "%s{provider=%q} %v\n", name, p.Name, v(p))
		}
	}
	series("llmgate_provider_calls_total", "counter", "Calls made to each provider.", func(p router.ProviderStats) any { return p.Calls })
	series("llmgate_provider_errors_total", "counter", "Failed calls per provider.", func(p router.ProviderStats) any { return p.Errors })
	series("llmgate_provider_skipped_total", "counter", "Requests that skipped a provider with an open breaker.", func(p router.ProviderStats) any { return p.Skipped })
	series("llmgate_provider_latency_seconds_total", "counter", "Summed call latency per provider.", func(p router.ProviderStats) any { return fmt.Sprintf("%f", p.Latency.Seconds()) })
	series("llmgate_provider_breaker_open", "gauge", "1 while a provider's circuit breaker is open.", func(p router.ProviderStats) any {
		if p.Open {
			return 1
		}
		return 0
	})
}

// handleStatus is a tiny human-readable status page.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	tokens, cost, calls := s.deps.Meter.Totals()
	cs := s.deps.Cache.Stats()
	_, shared := s.flight.Stats()
	provs := make([]map[string]any, 0)
	for _, p := range s.deps.Router.Stats() {
		var avgMS float64
		if p.Calls > 0 {
			avgMS = float64(p.Latency.Milliseconds()) / float64(p.Calls)
		}
		provs = append(provs, map[string]any{
			"name": p.Name, "calls": p.Calls, "errors": p.Errors,
			"avg_latency_ms": avgMS, "breaker_open": p.Open,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":      "llmgate",
		"calls":        calls,
		"tokens":       tokens,
		"cost_usd":     cost,
		"cache":        map[string]any{"hits": cs.Hits, "misses": cs.Misses, "entries": cs.Entries, "coalesced": shared},
		"providers":    provs,
		"guardrails":   s.deps.Config.Guardrails,
		"max_attempts": s.deps.Config.Retry.MaxAttempts,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg}})
}
