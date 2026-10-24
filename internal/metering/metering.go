// Package metering records per-request token and cost accounting. The default
// implementation keeps an in-memory tally and logs each record; swap in a
// Postgres-backed Meter (see NewPostgres below) once you add a DB driver.
package metering

import (
	"log/slog"
	"sync"
	"time"

	"github.com/harshakumar25/llmgate/internal/providers"
)

// Record is one metered call.
type Record struct {
	Time     time.Time
	APIKey   string
	Provider string
	Model    string
	Usage    providers.Usage
	CostUSD  float64
}

// Meter records usage. Implementations must be safe for concurrent use.
type Meter interface {
	Record(r Record)
	// Totals returns aggregate token and cost figures, for the status page.
	Totals() (tokens int, costUSD float64, calls int)
}

// pricePer1K is a tiny static price table (USD per 1K total tokens). Adjust to
// real provider pricing, or move it into config. Unknown models cost 0.
var pricePer1K = map[string]float64{
	"gpt-4o-mini": 0.00075,
	"gpt-4o":      0.0125,
	"llama3.2":    0, // local
}

// Cost estimates the USD cost of a usage record for a model.
func Cost(model string, u providers.Usage) float64 {
	return pricePer1K[model] * float64(u.TotalTokens) / 1000.0
}

// memMeter is the dependency-free default.
type memMeter struct {
	mu     sync.Mutex
	tokens int
	cost   float64
	calls  int
}

// New returns the in-memory meter.
func New() Meter { return &memMeter{} }

func (m *memMeter) Record(r Record) {
	m.mu.Lock()
	m.tokens += r.Usage.TotalTokens
	m.cost += r.CostUSD
	m.calls++
	m.mu.Unlock()
	slog.Info("metered",
		"provider", r.Provider,
		"model", r.Model,
		"total_tokens", r.Usage.TotalTokens,
		"cost_usd", r.CostUSD,
		"api_key", mask(r.APIKey),
	)
}

func (m *memMeter) Totals() (int, float64, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens, m.cost, m.calls
}

func mask(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

// NewPostgres is the seam for persistent metering.
//
// TODO (Day 6): implement a Postgres-backed Meter.
//   1. go get github.com/jackc/pgx/v5
//   2. Create table:
//        CREATE TABLE usage (
//          id BIGSERIAL PRIMARY KEY, ts TIMESTAMPTZ NOT NULL DEFAULT now(),
//          api_key TEXT, provider TEXT, model TEXT,
//          prompt_tokens INT, completion_tokens INT, total_tokens INT,
//          cost_usd NUMERIC(12,6)
//        );
//   3. INSERT on Record; SUM(...) on Totals.
//
// Until then this returns the in-memory meter so the app runs with or without
// a database configured.
func NewPostgres(dsn string) Meter {
	if dsn == "" {
		return New()
	}
	slog.Warn("metering: Postgres meter not yet implemented, using in-memory", "dsn_set", true)
	return New()
}
