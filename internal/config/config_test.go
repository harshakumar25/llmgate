package config

import (
	"testing"
	"time"
)

// TestExampleConfigLoads keeps config.example.yaml and the parser in step:
// every documented key must be understood.
func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "ak")
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, p := range cfg.Providers {
		names = append(names, p.Name)
	}
	if len(names) != 3 || names[0] != "openai" || names[1] != "anthropic" || names[2] != "ollama" {
		t.Fatalf("providers = %v", names)
	}
	if m := cfg.Providers[0].Models; len(m) != 1 || m[0] != "gpt-4o" {
		t.Fatalf("openai models = %q", m)
	}
	if m := cfg.Providers[2].Models; len(m) != 1 || m[0] != "*" {
		t.Fatalf("ollama models = %q (inline comment or quotes not stripped?)", m)
	}
	if !cfg.ModelFallback || cfg.UsageLog != "" || cfg.Budget.USDDaily != 0 {
		t.Fatalf("fallback=%v usage_log=%q budget=%+v", cfg.ModelFallback, cfg.UsageLog, cfg.Budget)
	}
	if cfg.Providers[1].APIKey != "ak" {
		t.Fatalf("api_key not expanded: %q", cfg.Providers[1].APIKey)
	}
	r := cfg.Retry
	if r.MaxBackoff != 2*time.Second || r.BreakerFailures != 5 || r.BreakerCooldown != 30*time.Second {
		t.Fatalf("retry = %+v", r)
	}
	if cfg.Cache.MaxEntries != 10000 || cfg.Cache.TTL != 5*time.Minute {
		t.Fatalf("cache = %+v", cfg.Cache)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("LLMGATE_RATE_LIMIT_RPM", "120")
	t.Setenv("LLMGATE_UPSTREAM_TIMEOUT_S", "300")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimit.PerMinute != 120 || cfg.Timeout != 300*time.Second {
		t.Fatalf("rate=%d timeout=%s", cfg.RateLimit.PerMinute, cfg.Timeout)
	}
}

func TestInlineComments(t *testing.T) {
	cases := map[string]string{
		`api_key: ${KEY}   # expanded from the environment`: "${KEY}",
		`model: "*"   # any model`:                          "*",
		`base_url: http://h/#frag`:                          "http://h/#frag", // no space before '#'
		`api_key: "abc # not a comment"`:                    "abc # not a comment",
		`addr: ":8080"`:                                     ":8080",
		`api_keys: # nothing`:                               "",
	}
	for in, want := range cases {
		if _, got := splitKV(in); got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}
}
