// Package metering records per-request token and cost accounting: lifetime
// totals for /metrics, and per-key, per-model usage for the current UTC day,
// which budgets and GET /v1/usage read. NewLedger also appends every record
// to a JSON-lines file and replays it on start, so spend survives restarts
// without a database.
package metering

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
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
	// Totals returns lifetime token and cost figures, for the status page.
	Totals() (tokens int, costUSD float64, calls int)
	// Usage returns apiKey's usage on the UTC day containing at.
	Usage(apiKey string, at time.Time) KeyUsage
}

// KeyUsage is one key's usage for one UTC day.
type KeyUsage struct {
	Key     string                `json:"key"` // KeyID, never the raw key
	Day     string                `json:"day"` // YYYY-MM-DD, UTC
	Calls   int                   `json:"calls"`
	Tokens  int                   `json:"tokens"`
	CostUSD float64               `json:"cost_usd"`
	Models  map[string]ModelUsage `json:"models"`
}

// ModelUsage is the per-model part of a KeyUsage.
type ModelUsage struct {
	Calls            int     `json:"calls"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// KeyID is a stable, non-reversible name for an API key, safe to log and to
// write to disk. Requests without a key share the "anonymous" bucket.
func KeyID(apiKey string) string {
	if apiKey == "" {
		return "anonymous"
	}
	sum := sha256.Sum256([]byte(apiKey))
	return "key_" + hex.EncodeToString(sum[:6])
}

func day(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// memMeter keeps lifetime totals plus today's usage per key. Older days
// only feed the totals, so memory is bounded by the number of keys.
type memMeter struct {
	mu     sync.Mutex
	tokens int
	cost   float64
	calls  int
	keys   map[string]*KeyUsage

	log    *os.File // nil without a ledger file
	logErr bool
}

// New returns the in-memory meter.
func New() Meter { return &memMeter{keys: make(map[string]*KeyUsage)} }

// ledgerLine is the on-disk form of a Record.
type ledgerLine struct {
	Time             time.Time `json:"ts"`
	Key              string    `json:"key"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	CostUSD          float64   `json:"cost_usd"`
}

// tokens trusts the provider's total, which can exceed prompt+completion
// (reasoning or cached tokens), and falls back to the sum when it is absent.
func (l ledgerLine) tokens() int {
	if l.TotalTokens > 0 {
		return l.TotalTokens
	}
	return l.PromptTokens + l.CompletionTokens
}

// NewLedger returns a meter that appends each record to path as one JSON
// line and starts from the totals already in it. An empty path is New().
func NewLedger(path string) (Meter, error) {
	m := New().(*memMeter)
	if path == "" {
		return m, nil
	}
	if err := m.replay(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("metering: open ledger: %w", err)
	}
	m.log = f
	return m, nil
}

func (m *memMeter) replay(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("metering: read ledger: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n, bad := 0, 0
	for sc.Scan() {
		var l ledgerLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			bad++ // e.g. a line cut short by a crash; skip it
			continue
		}
		m.add(l)
		n++
	}
	slog.Info("metering: ledger replayed", "path", path, "records", n, "skipped", bad, "cost_usd", m.cost)
	return sc.Err()
}

func (m *memMeter) Record(r Record) {
	l := ledgerLine{
		Time:             r.Time.UTC(),
		Key:              KeyID(r.APIKey),
		Provider:         r.Provider,
		Model:            r.Model,
		PromptTokens:     r.Usage.PromptTokens,
		CompletionTokens: r.Usage.CompletionTokens,
		TotalTokens:      r.Usage.TotalTokens,
		CostUSD:          r.CostUSD,
	}
	m.mu.Lock()
	m.add(l)
	if m.log != nil {
		b, _ := json.Marshal(l)
		if _, err := m.log.Write(append(b, '\n')); err != nil && !m.logErr {
			m.logErr = true
			slog.Error("metering: ledger write failed; usage is still counted in memory", "err", err)
		}
	}
	m.mu.Unlock()
	slog.Info("metered",
		"provider", r.Provider,
		"model", r.Model,
		"total_tokens", r.Usage.TotalTokens,
		"cost_usd", r.CostUSD,
		"key", l.Key,
	)
}

// add folds one line into the totals. The caller holds m.mu (or owns m).
func (m *memMeter) add(l ledgerLine) {
	tokens := l.tokens()
	m.tokens += tokens
	m.cost += l.CostUSD
	m.calls++

	d := day(l.Time)
	u := m.keys[l.Key]
	if u == nil || u.Day < d {
		u = &KeyUsage{Key: l.Key, Day: d, Models: map[string]ModelUsage{}}
		m.keys[l.Key] = u
	} else if u.Day > d {
		return // an older day, replayed out of order: totals only
	}
	u.Calls++
	u.Tokens += tokens
	u.CostUSD += l.CostUSD
	mu := u.Models[l.Model]
	mu.Calls++
	mu.PromptTokens += l.PromptTokens
	mu.CompletionTokens += l.CompletionTokens
	mu.CostUSD += l.CostUSD
	u.Models[l.Model] = mu
}

func (m *memMeter) Totals() (int, float64, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens, m.cost, m.calls
}

func (m *memMeter) Usage(apiKey string, at time.Time) KeyUsage {
	id, d := KeyID(apiKey), day(at)
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.keys[id]
	if u == nil || u.Day != d {
		return KeyUsage{Key: id, Day: d, Models: map[string]ModelUsage{}}
	}
	out := *u
	out.Models = make(map[string]ModelUsage, len(u.Models))
	for k, v := range u.Models {
		out.Models[k] = v
	}
	return out
}
