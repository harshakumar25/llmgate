package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harshakumar25/llmgate/internal/cache"
	"github.com/harshakumar25/llmgate/internal/config"
	"github.com/harshakumar25/llmgate/internal/guardrails"
	"github.com/harshakumar25/llmgate/internal/metering"
	"github.com/harshakumar25/llmgate/internal/providers"
	"github.com/harshakumar25/llmgate/internal/router"
)

// slowProvider simulates a model that takes a while to answer.
type slowProvider struct {
	delay time.Duration
	calls atomic.Int32
	fail  atomic.Bool
}

func (p *slowProvider) Name() string { return "slow" }

func (p *slowProvider) Chat(ctx context.Context, req providers.Request) (*providers.Response, error) {
	p.calls.Add(1)
	time.Sleep(p.delay)
	if p.fail.Load() {
		return nil, &providers.StatusError{Provider: "slow", Code: 503}
	}
	return &providers.Response{
		Content:  "echo: " + req.Messages[len(req.Messages)-1].Content,
		Model:    "m",
		Provider: "slow",
		Usage:    providers.Usage{TotalTokens: 10},
	}, nil
}

func newTestServer(t testing.TB, p providers.Provider, mutate func(*config.Config)) *httptest.Server {
	t.Helper()
	return newTestServerDeps(t, p, mutate, nil)
}

func newTestServerDeps(t testing.TB, p providers.Provider, mutate func(*config.Config), deps func(*Deps)) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Retry.Backoff = time.Millisecond
	if mutate != nil {
		mutate(cfg)
	}
	d := Deps{
		Config:     cfg,
		Router:     router.New([]providers.Provider{p}, cfg.Retry),
		Guardrails: guardrails.New(cfg.Guardrails),
		Meter:      metering.New(),
		Cache:      cache.NewMemory(cfg.Cache.MaxEntries),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if deps != nil {
		deps(&d)
	}
	ts := httptest.NewServer(New(d).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func chat(t testing.TB, url, prompt string, header ...string) (int, string) {
	t.Helper()
	body := `{"messages":[{"role":"user","content":"` + prompt + `"}]}`
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat", strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("X-LLMGate-Cache")
}

func TestBurstOfIdenticalRequestsMakesOneUpstreamCall(t *testing.T) {
	p := &slowProvider{delay: 200 * time.Millisecond}
	ts := newTestServer(t, p, nil)

	const n = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	kinds := map[string]int{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, kind := chat(t, ts.URL, "same question")
			if code != http.StatusOK {
				t.Errorf("status %d", code)
			}
			mu.Lock()
			kinds[kind]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if c := p.calls.Load(); c != 1 {
		t.Fatalf("upstream calls = %d for %d identical requests, want 1", c, n)
	}
	if kinds["miss"] != 1 || kinds["coalesced"]+kinds["hit"] != n-1 {
		t.Fatalf("cache headers = %v", kinds)
	}
	t.Logf("%d requests -> 1 upstream call (%v)", n, kinds)
}

func TestCacheHitAndBypass(t *testing.T) {
	p := &slowProvider{}
	ts := newTestServer(t, p, nil)

	if _, kind := chat(t, ts.URL, "q"); kind != "miss" {
		t.Fatalf("first = %q", kind)
	}
	if _, kind := chat(t, ts.URL, "q"); kind != "hit" {
		t.Fatalf("second = %q", kind)
	}
	if _, kind := chat(t, ts.URL, "q", "Cache-Control", "no-cache"); kind != "bypass" {
		t.Fatalf("no-cache = %q", kind)
	}
	if c := p.calls.Load(); c != 2 {
		t.Fatalf("upstream calls = %d, want 2", c)
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	ts := newTestServer(t, &slowProvider{}, nil)
	code, _ := chat(t, ts.URL, strings.Repeat("a", maxBodyBytes+1))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", code)
	}
}

func TestOpenBreakerFailsFastWith503(t *testing.T) {
	p := &slowProvider{}
	p.fail.Store(true)
	ts := newTestServer(t, p, func(c *config.Config) {
		c.Retry.MaxAttempts = 1
		c.Retry.BreakerFailures = 2
	})
	chat(t, ts.URL, "a")
	chat(t, ts.URL, "b")
	code, _ := chat(t, ts.URL, "c")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once the breaker is open", code)
	}
	if c := p.calls.Load(); c != 2 {
		t.Fatalf("upstream calls = %d, want 2", c)
	}
}

func TestMetricsExposeEfficiencyCounters(t *testing.T) {
	ts := newTestServer(t, &slowProvider{}, nil)
	chat(t, ts.URL, "q")
	chat(t, ts.URL, "q")
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		"llmgate_cache_hits_total 1",
		"llmgate_calls_total 1",
		`llmgate_provider_calls_total{provider="slow"} 1`,
		`llmgate_provider_breaker_open{provider="slow"} 0`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics missing %q:\n%s", want, b)
		}
	}
}

// BenchmarkHotPrompt measures the gateway's own overhead on a cached prompt:
// decode, guardrails, cache lookup and encode, with no upstream call. It drives
// the handler directly so the number is the gateway, not the loopback socket.
func BenchmarkHotPrompt(b *testing.B) {
	cfg := config.Default()
	srv := New(Deps{
		Config:     cfg,
		Router:     router.New([]providers.Provider{&slowProvider{}}, cfg.Retry),
		Guardrails: guardrails.New(cfg.Guardrails),
		Meter:      metering.New(),
		Cache:      cache.NewMemory(cfg.Cache.MaxEntries),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h := srv.Handler()
	const body = `{"messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"What is the capital of France?"}]}`
	serve := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(body)))
		return rec.Code
	}
	serve()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if serve() != http.StatusOK {
				b.Error("non-200")
				return
			}
		}
	})
}

// streamProvider streams fixed fragments, like a real model.
type streamProvider struct {
	slowProvider
	parts []string
}

func (p *streamProvider) ChatStream(_ context.Context, _ providers.Request, onDelta func(string) error) (*providers.Response, error) {
	p.calls.Add(1)
	text := ""
	for _, s := range p.parts {
		if err := onDelta(s); err != nil {
			return nil, err
		}
		text += s
	}
	return &providers.Response{Content: text, Model: "m", Provider: "stream", Usage: providers.Usage{TotalTokens: 7}}, nil
}

// streamChat posts a streaming request and returns the data payloads.
func streamChat(t *testing.T, url, prompt string) (int, string, []string) {
	t.Helper()
	body := `{"stream":true,"messages":[{"role":"user","content":"` + prompt + `"}]}`
	resp, err := http.Post(url+"/v1/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var events []string
	for _, line := range strings.Split(string(b), "\n") {
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			events = append(events, d)
		}
	}
	return resp.StatusCode, resp.Header.Get("X-LLMGate-Cache"), events
}

func TestStreamingProxiesDeltasThenServesFromCache(t *testing.T) {
	p := &streamProvider{parts: []string{"Hel", "lo"}}
	ts := newTestServer(t, p, nil)

	code, kind, ev := streamChat(t, ts.URL, "q")
	want := []string{`{"delta":"Hel"}`, `{"delta":"lo"}`}
	if code != 200 || kind != "miss" || len(ev) != 4 || ev[0] != want[0] || ev[1] != want[1] || ev[3] != "[DONE]" {
		t.Fatalf("code=%d kind=%q events=%q", code, kind, ev)
	}
	if !strings.Contains(ev[2], `"done":true`) || !strings.Contains(ev[2], `"total_tokens":7`) {
		t.Fatalf("final event = %s", ev[2])
	}

	// The finished stream filled the cache: a streamed repeat and a plain
	// repeat both skip the provider.
	if _, kind, ev := streamChat(t, ts.URL, "q"); kind != "hit" || ev[0] != `{"delta":"Hello"}` {
		t.Fatalf("streamed repeat: kind=%q events=%q", kind, ev)
	}
	if _, kind := chat(t, ts.URL, "q"); kind != "hit" {
		t.Fatalf("plain repeat: kind=%q", kind)
	}
	if c := p.calls.Load(); c != 1 {
		t.Fatalf("upstream calls = %d, want 1", c)
	}
}

func TestStreamUpstreamFailureBeforeOutputIsAStatus(t *testing.T) {
	p := &slowProvider{}
	p.fail.Store(true)
	ts := newTestServer(t, p, func(c *config.Config) { c.Retry.MaxAttempts = 1 })
	code, _, _ := streamChat(t, ts.URL, "q")
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 before any output", code)
	}
}

func TestRateLimitPerKey(t *testing.T) {
	ts := newTestServer(t, &slowProvider{}, func(c *config.Config) {
		c.RateLimit = config.RateLimit{PerMinute: 60, Burst: 2}
		c.APIKeys = []string{"alice", "bob"}
	})
	codes := func(key string, n int) []int {
		var out []int
		for i := 0; i < n; i++ {
			code, _ := chat(t, ts.URL, fmt.Sprint(key, i), "Authorization", "Bearer "+key)
			out = append(out, code)
		}
		return out
	}
	if got := codes("alice", 3); got[0] != 200 || got[1] != 200 || got[2] != http.StatusTooManyRequests {
		t.Fatalf("alice = %v, want 200 200 429", got)
	}
	if got := codes("bob", 1); got[0] != 200 {
		t.Fatalf("bob = %v; keys must not share a bucket", got)
	}
}

func TestRateLimitHeaders(t *testing.T) {
	ts := newTestServer(t, &slowProvider{}, func(c *config.Config) {
		c.RateLimit = config.RateLimit{PerMinute: 60, Burst: 1}
	})
	post := func() *http.Response {
		resp, err := http.Post(ts.URL+"/v1/chat", "application/json",
			strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := post(); r.Header.Get("X-RateLimit-Limit") != "1" || r.Header.Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("headers = %v", r.Header)
	}
	if r := post(); r.StatusCode != 429 || r.Header.Get("Retry-After") != "1" {
		t.Fatalf("second: %d Retry-After=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
}

// echoProvider replies with the prompt verbatim.
type echoProvider struct{ slowProvider }

func (p *echoProvider) Chat(_ context.Context, req providers.Request) (*providers.Response, error) {
	return &providers.Response{Content: "contact me at " + req.Messages[0].Content, Provider: "echo"}, nil
}

func TestOutputRedaction(t *testing.T) {
	ts := newTestServer(t, &echoProvider{}, func(c *config.Config) {
		c.Guardrails.RedactPII = false // let the address reach the model...
		c.Guardrails.RedactOutput = true
	})
	resp, err := http.Post(ts.URL+"/v1/chat", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"a@b.co"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "a@b.co") || !strings.Contains(string(b), "[REDACTED_EMAIL]") {
		t.Fatalf("...but not back out: %s", b)
	}
}
