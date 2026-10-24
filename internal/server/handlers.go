package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/harshakumar25/llmgate/internal/cache"
	"github.com/harshakumar25/llmgate/internal/metering"
	"github.com/harshakumar25/llmgate/internal/providers"
)

// cacheTTL is how long completions are cached. Move to config as needed.
const cacheTTL = 5 * time.Minute

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req providers.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	// 2. Cache: return a prior identical completion if fresh.
	key := cache.Key(req)
	if cached, ok := s.deps.Cache.Get(key); ok {
		w.Header().Set("X-LLMGate-Cache", "hit")
		writeJSON(w, http.StatusOK, cached)
		return
	}

	// 3. Route across providers with failover + retry.
	resp, err := s.deps.Router.Route(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("upstream failed: %v", err))
		return
	}

	// 4. Meter token + cost usage.
	s.deps.Meter.Record(metering.Record{
		Time:     time.Now(),
		APIKey:   bearerToken(r),
		Provider: resp.Provider,
		Model:    resp.Model,
		Usage:    resp.Usage,
		CostUSD:  metering.Cost(resp.Model, resp.Usage),
	})

	// 5. Cache and return.
	s.deps.Cache.Set(key, resp, cacheTTL)
	w.Header().Set("X-LLMGate-Cache", "miss")
	writeJSON(w, http.StatusOK, resp)
}

// handleMetrics emits a minimal Prometheus text-format exposition. Replace the
// hand-rolled counters with the official client_golang collectors when you add
// the dependency.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	tokens, cost, calls := s.deps.Meter.Totals()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP llmgate_calls_total Total chat calls served.\n")
	fmt.Fprintf(w, "# TYPE llmgate_calls_total counter\n")
	fmt.Fprintf(w, "llmgate_calls_total %d\n", calls)
	fmt.Fprintf(w, "# HELP llmgate_tokens_total Total tokens across all calls.\n")
	fmt.Fprintf(w, "# TYPE llmgate_tokens_total counter\n")
	fmt.Fprintf(w, "llmgate_tokens_total %d\n", tokens)
	fmt.Fprintf(w, "# HELP llmgate_cost_usd_total Estimated spend in USD.\n")
	fmt.Fprintf(w, "# TYPE llmgate_cost_usd_total counter\n")
	fmt.Fprintf(w, "llmgate_cost_usd_total %f\n", cost)
}

// handleStatus is a tiny human-readable status page.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	tokens, cost, calls := s.deps.Meter.Totals()
	writeJSON(w, http.StatusOK, map[string]any{
		"service":     "llmgate",
		"calls":       calls,
		"tokens":      tokens,
		"cost_usd":    cost,
		"guardrails":  s.deps.Config.Guardrails,
		"provider":    s.deps.Config.Providers[0].Name,
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
