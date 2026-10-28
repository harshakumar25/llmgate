// Package server wires the HTTP surface of the gateway: the chat endpoint,
// health and status, a minimal Prometheus-style metrics endpoint, and API-key
// auth middleware.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/harshakumar25/llmgate/internal/cache"
	"github.com/harshakumar25/llmgate/internal/config"
	"github.com/harshakumar25/llmgate/internal/flight"
	"github.com/harshakumar25/llmgate/internal/guardrails"
	"github.com/harshakumar25/llmgate/internal/metering"
	"github.com/harshakumar25/llmgate/internal/providers"
	"github.com/harshakumar25/llmgate/internal/router"
)

// Deps are the server's collaborators, injected for testability.
type Deps struct {
	Config     *config.Config
	Router     *router.Router
	Guardrails *guardrails.Guardrails
	Meter      metering.Meter
	Cache      cache.Cache
	Logger     *slog.Logger
}

// Server holds dependencies and exposes an http.Handler.
type Server struct {
	deps     Deps
	apiKeys  map[string]struct{}
	cacheTTL time.Duration
	flight   flight.Group[*providers.Response]
}

// New builds a Server.
func New(d Deps) *Server {
	keys := make(map[string]struct{}, len(d.Config.APIKeys))
	for _, k := range d.Config.APIKeys {
		keys[k] = struct{}{}
	}
	return &Server{deps: d, apiKeys: keys, cacheTTL: d.Config.Cache.TTL}
}

// Handler returns the configured router with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.Handle("POST /v1/chat", s.authMiddleware(http.HandlerFunc(s.handleChat)))
	return s.logMiddleware(mux)
}

// authMiddleware enforces a bearer API key when any keys are configured. With
// no keys configured the gateway is open (fine for local dev).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.apiKeys) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		key := bearerToken(r)
		if _, ok := s.apiKeys[key]; !ok {
			writeError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.deps.Logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"cache", sw.Header().Get("X-LLMGate-Cache"),
			"ms", time.Since(start).Milliseconds(),
		)
	})
}

// statusWriter records the status code a handler wrote, for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}
