// Package eval is a tiny golden-set evaluation harness. It runs a set of chat
// cases through any ChatFunc, scores each answer against expected substrings,
// and reports pass rate and latency. The CI workflow fails the build when the
// pass rate drops below a threshold or latency regresses — that gate is the
// point of the package.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/harshakumar25/llmgate/internal/providers"
)

// Case is one evaluation item.
type Case struct {
	Name           string              `json:"name"`
	Messages       []providers.Message `json:"messages"`
	ExpectContains []string            `json:"expect_contains"`
}

// ChatFunc performs a completion for a case. It is satisfied by the router,
// an HTTP client against a live gateway, or a stub in tests.
type ChatFunc func(ctx context.Context, req providers.Request) (*providers.Response, error)

// CaseResult is the outcome of a single case.
type CaseResult struct {
	Name    string
	Passed  bool
	Latency time.Duration
	Detail  string
}

// Report aggregates case results.
type Report struct {
	Results    []CaseResult
	Passed     int
	Total      int
	MaxLatency time.Duration
}

// PassRate returns the fraction of cases that passed, in [0,1].
func (r Report) PassRate() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Passed) / float64(r.Total)
}

// LoadCases reads a golden set from a JSON file.
func LoadCases(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []Case
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	return cases, nil
}

// Run executes every case through chat and scores it.
func Run(ctx context.Context, cases []Case, chat ChatFunc) Report {
	rep := Report{Total: len(cases)}
	for _, c := range cases {
		start := time.Now()
		resp, err := chat(ctx, providers.Request{Messages: c.Messages})
		lat := time.Since(start)
		if lat > rep.MaxLatency {
			rep.MaxLatency = lat
		}
		res := CaseResult{Name: c.Name, Latency: lat}
		switch {
		case err != nil:
			res.Detail = "error: " + err.Error()
		case score(resp.Content, c.ExpectContains):
			res.Passed = true
			rep.Passed++
		default:
			res.Detail = fmt.Sprintf("missing expected substrings in %q", trunc(resp.Content))
		}
		rep.Results = append(rep.Results, res)
	}
	return rep
}

// score passes when the answer contains every expected substring (case-insensitive).
func score(answer string, expect []string) bool {
	low := strings.ToLower(answer)
	for _, e := range expect {
		if !strings.Contains(low, strings.ToLower(e)) {
			return false
		}
	}
	return true
}

func trunc(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
