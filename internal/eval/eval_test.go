package eval

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/harshakumar25/llmgate/internal/providers"
)

// stubChat answers the golden cases deterministically, so CI does not depend on
// a live model. It stands in for the router during the quality gate. When you
// run `make eval` against a real gateway, swap this for an HTTP ChatFunc.
func stubChat(_ context.Context, req providers.Request) (*providers.Response, error) {
	q := strings.ToLower(req.Messages[len(req.Messages)-1].Content)
	answer := "I don't know."
	switch {
	case strings.Contains(q, "capital of france"):
		answer = "Paris"
	case strings.Contains(q, "7 multiplied by 6"):
		answer = "42"
	case strings.Contains(q, "curly braces"):
		answer = "That format is JSON."
	}
	return &providers.Response{Content: answer, Provider: "stub", Model: "stub"}, nil
}

// Thresholds the quality gate enforces. Tune these as the golden set grows.
const (
	minPassRate = 1.0
	maxLatency  = 2 * time.Second
)

func TestGoldenSet(t *testing.T) {
	cases, err := LoadCases("testdata/golden.json")
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("golden set is empty")
	}

	rep := Run(context.Background(), cases, stubChat)

	for _, r := range rep.Results {
		if !r.Passed {
			t.Errorf("case %q failed: %s", r.Name, r.Detail)
		}
	}
	if got := rep.PassRate(); got < minPassRate {
		t.Errorf("pass rate %.2f below threshold %.2f", got, minPassRate)
	}
	if rep.MaxLatency > maxLatency {
		t.Errorf("max latency %s exceeds budget %s", rep.MaxLatency, maxLatency)
	}
	t.Logf("eval: %d/%d passed, max latency %s", rep.Passed, rep.Total, rep.MaxLatency)
}
