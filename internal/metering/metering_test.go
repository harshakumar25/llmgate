package metering

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harshakumar25/llmgate/internal/config"
	"github.com/harshakumar25/llmgate/internal/providers"
)

func rec(key, model string, at time.Time, in, out int, cost float64) Record {
	return Record{Time: at, APIKey: key, Provider: "openai", Model: model,
		Usage: providers.Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out}, CostUSD: cost}
}

func TestPerKeyPerModelDailyUsage(t *testing.T) {
	m := New()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	m.Record(rec("alice", "gpt-4o-mini", now, 100, 50, 0.01))
	m.Record(rec("alice", "gpt-4o", now, 10, 5, 0.02))
	m.Record(rec("bob", "gpt-4o-mini", now, 1, 1, 0.5))

	u := m.Usage("alice", now)
	if u.Calls != 2 || u.Tokens != 165 || u.Models["gpt-4o-mini"].CompletionTokens != 50 {
		t.Fatalf("alice = %+v", u)
	}
	if u.Key == "alice" || !strings.HasPrefix(u.Key, "key_") {
		t.Fatalf("raw key leaked into usage: %q", u.Key)
	}
	if got := m.Usage("alice", now.Add(24*time.Hour)); got.Calls != 0 {
		t.Fatalf("tomorrow should start at zero: %+v", got)
	}
	if tokens, _, calls := m.Totals(); tokens != 167 || calls != 3 {
		t.Fatalf("totals = %d tokens, %d calls", tokens, calls)
	}
}

func TestLedgerSurvivesRestartAndHidesKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	now := time.Now()

	m1, err := NewLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	m1.Record(rec("sk-secret-123", "gpt-4o-mini", now, 100, 100, 0.25))
	m1.Record(rec("sk-secret-123", "gpt-4o-mini", now, 100, 100, 0.25))

	// Simulate a crash mid-write: a truncated last line must be skipped.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"ts":"2026-10-28T`)
	f.Close()

	m2, err := NewLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if u := m2.Usage("sk-secret-123", now); u.Calls != 2 || u.CostUSD != 0.5 {
		t.Fatalf("after restart usage = %+v, want 2 calls / $0.50", u)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "sk-secret") {
		t.Fatal("raw API key written to the ledger")
	}
}

func TestPrices(t *testing.T) {
	u := providers.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}
	p := NewPrices([]config.Provider{{Name: "anthropic", PriceIn: 1, PriceOut: 5}})

	cases := []struct {
		provider, model string
		want            float64
	}{
		{"openai", "gpt-4o-mini", 0.75},
		{"openai", "gpt-4o-mini-2024-07-18", 0.75}, // dated snapshot
		{"openai", "gpt-4o", 12.5},
		{"anthropic", "any-model", 6}, // config override
		{"ollama", "gpt-4o-mini", 0},  // local is free even under a priced name
		{"openai", "unknown", 0},
	}
	for _, c := range cases {
		if got := p.Cost(c.provider, c.model, u); got != c.want {
			t.Errorf("%s/%s = %v, want %v", c.provider, c.model, got, c.want)
		}
	}
	var nilPrices *Prices
	if nilPrices.Cost("openai", "gpt-4o", u) != 12.5 {
		t.Error("nil Prices should still use the default table")
	}
}
