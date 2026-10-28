// Package server wires the HTTP surface of the gateway: the native and
// OpenAI-compatible chat endpoints, models and usage, health and status, a
// Prometheus-style metrics endpoint, and the request-ID, logging, auth and
// rate-limit middleware.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/harshakumar25/llmgate/internal/cache"
	"github.com/harshakumar25/llmgate/internal/config"
	"github.com/harshakumar25/llmgate/internal/flight"
	"github.com/harshakumar25/llmgate/internal/guardrails"
	"github.com/harshakumar25/llmgate/internal/metering"
	"github.com/harshakumar25/llmgate/internal/providers"
	"github.com/harshakumar25/llmgate/internal/ratelimit"
	"github.com/harshakumar25/llmgate/internal/router"
)

// Deps are the server's collaborators, injected for testability.
type Deps struct {
	Config     *config.Config
	Router     *router.Router
	Guardrails *guardrails.Guardrails
	Classifier *guardrails.Classifier // nil when the model-based check is off
	Meter      metering.Meter
	Prices     *metering.Prices // nil prices with the built-in table only
	Cache      cache.Cache
	Logger     *slog.Logger
}

// Server holds dependencies and exposes an http.Handler.
type Server struct {
	deps     Deps
	apiKeys  map[string]struct{}
	cacheTTL time.Duration
	flight   flight.Group[*providers.Response]
	limiter  *ratelimit.Limiter // nil when rate limiting is off
}

// New builds a Server.
func New(d Deps) *Server {
	keys := make(map[string]struct{}, len(d.Config.APIKeys))
	for _, k := range d.Config.APIKeys {
		keys[k] = struct{}{}
	}
	s := &Server{deps: d, apiKeys: keys, cacheTTL: d.Config.Cache.TTL}
	if rl := d.Config.RateLimit; rl.PerMinute > 0 {
		s.limiter = ratelimit.New(rl.PerMinute, rl.Burst)
	}
	return s
}

// Handler returns the configured router with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /status", s.handleStatus)
	chat := func(h http.HandlerFunc) http.Handler { return s.authMiddleware(s.rateLimitMiddleware(h)) }
	mux.Handle("POST /v1/chat", chat(s.handleChat))
	mux.Handle("POST /v1/chat/completions", chat(s.handleChatCompletions))
	mux.Handle("GET /v1/models", s.authMiddleware(http.HandlerFunc(s.handleModels)))
	mux.Handle("GET /v1/usage", s.authMiddleware(http.HandlerFunc(s.handleUsage)))
	return s.requestIDMiddleware(s.logMiddleware(mux))
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

// rateLimitMiddleware spends one token per chat request from the caller's
// bucket, keyed by API key, or by client IP when no key is sent. It runs after
// auth, so unauthenticated junk cannot drain a real key's budget.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.limiter == nil {
			next.ServeHTTP(w, r)
			return
		}
		key := bearerToken(r)
		if key == "" {
			key = "ip:" + clientIP(r)
		}
		d := s.limiter.Allow(key)
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
		if !d.OK {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.RetryAfter.Seconds()))))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the connection's remote address. Behind a proxy every request
// shares the proxy's IP; use API keys there.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type ctxKey int

const requestIDKey ctxKey = 0

// requestIDMiddleware gives every request an ID: the caller's X-Request-ID
// when it is sane, otherwise a fresh random one. It is echoed back, logged,
// and used as the OpenAI completion id, so one value traces a request from
// client to gateway logs.
func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			var b [12]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// validRequestID accepts short IDs of letters, digits, '-' and '_', so a
// client cannot inject newlines or huge values into logs.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey).(string)
	return id
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.deps.Logger.Info("request",
			"id", requestID(r),
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

// Unwrap lets http.ResponseController reach the underlying writer, so
// streaming handlers can flush through this middleware.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}
