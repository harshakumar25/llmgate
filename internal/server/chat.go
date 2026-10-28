package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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

// format renders the result of the chat pipeline in one wire dialect. The
// pipeline (guardrails, cache, budget, coalescing, routing, metering) is the
// same for every endpoint; only the bytes on the wire differ.
type format interface {
	reply(w http.ResponseWriter, req providers.Request, resp *providers.Response)
	stream(w http.ResponseWriter, req providers.Request) streamWriter
}

// streamWriter writes one streamed reply. Nothing reaches the client until
// the first delta, so an early failure can still be sent with a real status.
type streamWriter interface {
	delta(text string) error
	done(resp *providers.Response)
	fail(msg string)
	started() bool
}

// handleChat serves the native endpoint, POST /v1/chat.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req providers.Request
	if !decodeBody(w, r, &req) {
		return
	}
	s.serveChat(w, r, req, nativeFormat{})
}

// decodeBody reads a size-capped JSON body into v, writing the error itself.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body exceeds %d bytes", maxBodyBytes))
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// serveChat runs the chat pipeline for one request and writes the reply in f.
func (s *Server) serveChat(w http.ResponseWriter, r *http.Request, req providers.Request, f format) {
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
	// force a fresh call with Cache-Control: no-cache. Hits are free, so
	// they are served even to a key that is over its budget.
	key := cache.Key(req)
	useCache := s.cacheTTL > 0 && !strings.Contains(r.Header.Get("Cache-Control"), "no-cache")
	if useCache {
		if cached, ok := s.deps.Cache.Get(key); ok {
			w.Header().Set("X-LLMGate-Cache", "hit")
			if req.Stream {
				sw := f.stream(w, req)
				if cached.Content != "" {
					_ = sw.delta(cached.Content)
				}
				sw.done(cached)
				return
			}
			f.reply(w, req, cached)
			return
		}
	}

	// 3. Budget: refuse new upstream spend once the key's daily cap is hit.
	apiKey := bearerToken(r)
	if msg := s.overBudget(apiKey); msg != "" {
		writeErrorCode(w, http.StatusTooManyRequests, "insufficient_quota", msg)
		return
	}

	if req.Stream {
		s.streamChat(w, r, req, key, useCache, f)
		return
	}

	// 4. Route across providers, coalescing identical requests already in
	// flight into one upstream call. The shared call runs detached from any
	// single client's context, so one caller hanging up does not fail the
	// others; the upstream timeout still bounds it.
	upstream := func() (*providers.Response, error) {
		resp, err := s.deps.Router.Route(context.WithoutCancel(r.Context()), req)
		if err != nil {
			return nil, err
		}
		// 5. Meter once per upstream call, not once per waiting client.
		s.meter(apiKey, resp)
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
	f.reply(w, req, resp)
}

// streamChat proxies a streamed completion. Streams are not coalesced (each
// client wants its own live tokens), but a finished stream fills the cache so
// later identical requests, streamed or not, skip the provider.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req providers.Request, key string, useCache bool, f format) {
	if useCache {
		w.Header().Set("X-LLMGate-Cache", "miss")
	} else {
		w.Header().Set("X-LLMGate-Cache", "bypass")
	}
	sw := f.stream(w, req)
	resp, err := s.deps.Router.RouteStream(r.Context(), req, sw.delta)
	if err != nil {
		if !sw.started() {
			s.writeRouteError(w, err)
			return
		}
		// Output already reached the client: the status line is gone, so the
		// failure travels as a final error event.
		sw.fail(err.Error())
		return
	}
	s.meter(bearerToken(r), resp)
	if useCache {
		s.deps.Cache.Set(key, s.redactOutput(resp), s.cacheTTL)
	}
	sw.done(resp)
}

func (s *Server) meter(apiKey string, resp *providers.Response) {
	s.deps.Meter.Record(metering.Record{
		Time:     time.Now(),
		APIKey:   apiKey,
		Provider: resp.Provider,
		Model:    resp.Model,
		Usage:    resp.Usage,
		CostUSD:  s.deps.Prices.Cost(resp.Provider, resp.Model, resp.Usage),
	})
}

// overBudget returns why apiKey may not spend more today, or "" if it may.
// The check runs before the call, so one request can overshoot the cap by its
// own cost; the next one is refused.
func (s *Server) overBudget(apiKey string) string {
	b := s.deps.Config.Budget
	if b.USDDaily <= 0 && b.TokensDaily <= 0 {
		return ""
	}
	u := s.deps.Meter.Usage(apiKey, time.Now())
	if b.USDDaily > 0 && u.CostUSD >= b.USDDaily {
		return fmt.Sprintf("daily budget of $%.2f reached ($%.4f spent); resets at 00:00 UTC", b.USDDaily, u.CostUSD)
	}
	if b.TokensDaily > 0 && u.Tokens >= b.TokensDaily {
		return fmt.Sprintf("daily budget of %d tokens reached (%d used); resets at 00:00 UTC", b.TokensDaily, u.Tokens)
	}
	return ""
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

// writeRouteError maps a router failure to a status code.
func (s *Server) writeRouteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, router.ErrAllOpen):
		w.Header().Set("Retry-After", strconv.Itoa(int(s.deps.Config.Retry.BreakerCooldown.Seconds())))
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, router.ErrUnknownModel):
		writeErrorCode(w, http.StatusNotFound, "model_not_found", err.Error())
	default:
		writeError(w, http.StatusBadGateway, fmt.Sprintf("upstream failed: %v", err))
	}
}

// nativeFormat is LLMGate's own compact dialect on POST /v1/chat.
type nativeFormat struct{}

func (nativeFormat) reply(w http.ResponseWriter, _ providers.Request, resp *providers.Response) {
	writeJSON(w, http.StatusOK, resp)
}

func (nativeFormat) stream(w http.ResponseWriter, _ providers.Request) streamWriter {
	return &nativeStream{sse: newSSE(w)}
}

// nativeStream writes:
//
//	data: {"delta":"Hel"}
//	data: {"delta":"lo"}
//	data: {"done":true,"model":"...","provider":"...","usage":{...}}
//	data: [DONE]
type nativeStream struct{ sse *sse }

func (n *nativeStream) delta(text string) error {
	return n.sse.event(map[string]string{"delta": text})
}

func (n *nativeStream) done(resp *providers.Response) {
	_ = n.sse.event(map[string]any{
		"done":     true,
		"model":    resp.Model,
		"provider": resp.Provider,
		"usage":    resp.Usage,
	})
	n.sse.finish()
}

func (n *nativeStream) fail(msg string) {
	_ = n.sse.event(map[string]any{"error": map[string]string{"message": msg}})
}

func (n *nativeStream) started() bool { return n.sse.started }
