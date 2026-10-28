package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/harshakumar25/llmgate/internal/metering"
	"github.com/harshakumar25/llmgate/internal/providers"
	"github.com/harshakumar25/llmgate/internal/router"
)

// sseWriter writes the gateway's server-sent event stream:
//
//	data: {"delta":"Hel"}
//	data: {"delta":"lo"}
//	data: {"done":true,"model":"...","provider":"...","usage":{...}}
//	data: [DONE]
//
// Headers are sent lazily on the first event, so an upstream failure before
// any output can still be reported as an ordinary JSON error with a status.
type sseWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
	cache   string
}

func newSSE(w http.ResponseWriter, cache string) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w), cache: cache}
}

func (s *sseWriter) start() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // keep nginx-style proxies from buffering
	h.Set("X-LLMGate-Cache", s.cache)
	s.w.WriteHeader(http.StatusOK)
}

func (s *sseWriter) event(v any) error {
	s.start()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sseWriter) delta(text string) error {
	return s.event(map[string]string{"delta": text})
}

func (s *sseWriter) done(resp *providers.Response) {
	_ = s.event(map[string]any{
		"done":     true,
		"model":    resp.Model,
		"provider": resp.Provider,
		"usage":    resp.Usage,
	})
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	_ = s.rc.Flush()
}

// streamCached replays a cached completion as a one-fragment stream, so a
// streaming client gets the same cache benefit as everyone else.
func (s *Server) streamCached(w http.ResponseWriter, resp *providers.Response) {
	sse := newSSE(w, "hit")
	if resp.Content != "" {
		_ = sse.delta(resp.Content)
	}
	sse.done(resp)
}

// streamChat proxies a streamed completion. Streams are not coalesced (each
// client wants its own live tokens), but a finished stream fills the cache so
// later identical requests, streamed or not, skip the provider.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req providers.Request, key string, useCache bool) {
	kind := "bypass"
	if useCache {
		kind = "miss"
	}
	sse := newSSE(w, kind)
	resp, err := s.deps.Router.RouteStream(r.Context(), req, sse.delta)
	if err != nil {
		if !sse.started {
			s.writeRouteError(w, err)
			return
		}
		// Output already reached the client: the status line is gone, so the
		// failure travels as a final error event.
		_ = sse.event(map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	s.deps.Meter.Record(metering.Record{
		Time:     time.Now(),
		APIKey:   bearerToken(r),
		Provider: resp.Provider,
		Model:    resp.Model,
		Usage:    resp.Usage,
		CostUSD:  metering.Cost(resp.Model, resp.Usage),
	})
	if useCache {
		s.deps.Cache.Set(key, s.redactOutput(resp), s.cacheTTL)
	}
	sse.done(resp)
}

// writeRouteError maps a router failure to a status code.
func (s *Server) writeRouteError(w http.ResponseWriter, err error) {
	if errors.Is(err, router.ErrAllOpen) {
		w.Header().Set("Retry-After", strconv.Itoa(int(s.deps.Config.Retry.BreakerCooldown.Seconds())))
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeError(w, http.StatusBadGateway, fmt.Sprintf("upstream failed: %v", err))
}
