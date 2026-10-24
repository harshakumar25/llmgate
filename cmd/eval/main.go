// Command eval runs the golden set against a live LLMGate instance and exits
// non-zero if the pass rate or latency budget is not met. Use it locally
// (`make eval`) once a real provider is wired; CI uses the deterministic
// package test instead.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/harshakumar25/llmgate/internal/eval"
	"github.com/harshakumar25/llmgate/internal/providers"
)

func main() {
	url := flag.String("url", "http://localhost:8080/v1/chat", "gateway chat endpoint")
	apiKey := flag.String("api-key", os.Getenv("LLMGATE_API_KEY"), "bearer API key, if required")
	goldens := flag.String("cases", "internal/eval/testdata/golden.json", "golden set JSON")
	minPass := flag.Float64("min-pass", 1.0, "minimum pass rate [0-1]")
	maxLatency := flag.Duration("max-latency", 10*time.Second, "per-case latency budget")
	flag.Parse()

	cases, err := eval.LoadCases(*goldens)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load cases:", err)
		os.Exit(2)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	chat := func(ctx context.Context, req providers.Request) (*providers.Response, error) {
		body, _ := json.Marshal(req)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, *url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if *apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+*apiKey)
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, data)
		}
		var out providers.Response
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}

	rep := eval.Run(context.Background(), cases, chat)

	fmt.Printf("\nLLMGate eval — %d cases\n", rep.Total)
	fmt.Println("-----------------------------------")
	for _, r := range rep.Results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
		}
		fmt.Printf("  [%s] %-22s %6dms  %s\n", status, r.Name, r.Latency.Milliseconds(), r.Detail)
	}
	fmt.Println("-----------------------------------")
	fmt.Printf("pass rate %.0f%%  max latency %dms\n\n", rep.PassRate()*100, rep.MaxLatency.Milliseconds())

	if rep.PassRate() < *minPass {
		fmt.Fprintf(os.Stderr, "FAIL: pass rate %.2f below %.2f\n", rep.PassRate(), *minPass)
		os.Exit(1)
	}
	if rep.MaxLatency > *maxLatency {
		fmt.Fprintf(os.Stderr, "FAIL: max latency %s exceeds %s\n", rep.MaxLatency, *maxLatency)
		os.Exit(1)
	}
	fmt.Println("OK")
}
