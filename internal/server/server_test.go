package server

import (
	"context"
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
	cfg := config.Default()
	cfg.Retry.Backoff = time.Millisecond
	if mutate != nil {
		mutate(cfg)
	}
	srv := New(Deps{
		Config:     cfg,
		Router:     router.New([]providers.Provider{p}, cfg.Retry),
		Guardrails: guardrails.New(cfg.Guardrails),
		Meter:      metering.New(),
		Cache:      cache.NewMemory(cfg.Cache.MaxEntries),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(srv.Handler())
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
