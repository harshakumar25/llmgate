<div align="center">

![LLMGate Banner](assets/llmgate-hero-banner.jpg)

# LLMGate

**Self-hostable LLM gateway in Go. Zero external dependencies.**

*One OpenAI-compatible endpoint in front of any model backend — with failover, caching, PII redaction, prompt-injection blocking, cost metering, and a golden-set CI gate that fails the build on quality regression.*

[![CI](https://github.com/harshakumar25/llmgate/actions/workflows/eval.yml/badge.svg)](https://github.com/harshakumar25/llmgate/actions/workflows/eval.yml)
[![Go 1.26](https://img.shields.io/badge/go-1.26-00ADD8?style=flat-square&logo=go&logoColor=white)](go.mod)
[![Zero Dependencies](https://img.shields.io/badge/dependencies-zero-brightgreen?style=flat-square)](go.mod)
[![CGO Disabled](https://img.shields.io/badge/CGO-disabled-success?style=flat-square)](Dockerfile)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue?style=flat-square)](LICENSE)
[![Docker](https://img.shields.io/badge/distroless-container-2496ED?style=flat-square&logo=docker)](Dockerfile)
[![Eval Gate](https://img.shields.io/badge/eval_gate-100%25_pass-brightgreen?style=flat-square)](internal/eval/eval_test.go)

<br/>

**[Architecture](#architecture) · [Data Flow](#data-flow) · [Modules](#module-breakdown) · [Provider Layer](#provider-layer--wire-format-translation) · [Security Layer](#security-layer) · [Eval Gate](#the-eval-gate) · [Metering](#metering--cost-accounting) · [Config](#configuration) · [Deployment](#deployment)**

</div>

---

## What is LLMGate?

LLMGate is a small, readable, self-hostable **LLM API gateway written in pure Go** with **zero external dependencies** at compile time. It presents a single OpenAI-compatible HTTP endpoint to client applications and handles all the plumbing that would otherwise pollute application code:

| Problem | What LLMGate does | Where in code |
|:---|:---|:---|
| Multiple providers, multiple wire formats | Normalized `Provider` interface; per-backend adapters | [`internal/providers`](internal/providers/) |
| Provider outages & transient errors | Ordered failover chain with per-attempt retry and backoff | [`internal/router`](internal/router/) |
| Repeated identical requests wasting tokens | SHA-256 content-addressed TTL cache (memory → Redis seam) | [`internal/cache`](internal/cache/) |
| Prompt injection & data exfiltration | Pre-flight heuristic regex scanner, returns `403` before hitting provider | [`internal/guardrails`](internal/guardrails/) |
| PII leaking into cloud APIs | In-place regex redaction of email, card, SSN, IP, phone | [`internal/guardrails`](internal/guardrails/) |
| No visibility into token spend | Per-call metering with USD estimate; Prometheus `/metrics` | [`internal/metering`](internal/metering/) |
| Model regressions invisible until production | Golden-set eval harness in CI; fails build on quality drop | [`internal/eval`](internal/eval/) |

> Most AI teams build a worse version of this in-house. LLMGate is a clean reference you can self-host, read in an afternoon, and extend.

---

## Architecture

<div align="center">

![LLMGate 3D Architecture](assets/llmgate-architecture-3d.jpg)

*Every inbound request passes through Auth → Guardrails → Cache → Router. Metering intercepts the response path. The shape is a strict pipeline: no goroutines per hop, no shared mutable state between stages.*

</div>

### System Topology

```
                        ┌──────────────────────────────────────────────────────────────┐
                        │                        LLMGate Process                        │
                        │                                                               │
  Client                │   HTTP Mux (net/http std ServeMux, Go 1.22 method routing)   │
  ──────────            │  ┌───────────────────────────────────────────────────────┐   │
  POST /v1/chat ──────► │  │ logMiddleware → authMiddleware → handleChat()         │   │
  GET  /healthz ──────► │  │ handleHealth()                                        │   │
  GET  /metrics ──────► │  │ handleMetrics()                                       │   │
  GET  /status  ──────► │  │ handleStatus()                                        │   │
                        │  └───────────────────────────────────────────────────────┘   │
                        │                                                               │
                        │   handleChat pipeline (sequential, same goroutine):           │
                        │                                                               │
                        │   ① json.NewDecoder(r.Body).Decode(&req)                     │
                        │   ② Guardrails.Apply(&req) ──── 403 on injection             │
                        │   ③ Cache.Get(SHA256(req)) ─── 200 on hit                    │
                        │   ④ Router.Route(ctx, req)                                   │
                        │      ├─ Provider[0].Chat() ── retry loop                     │
                        │      └─ Provider[N].Chat() ── failover                       │
                        │   ⑤ Meter.Record(usage, cost)                                │
                        │   ⑥ Cache.Set(key, resp, 5m)                                 │
                        │   ⑦ json.NewEncoder(w).Encode(resp)                          │
                        │                                                               │
                        └──────────────────────────────────────────────────────────────┘
                                                         │
                                       ┌─────────────────┴─────────────────┐
                                       ▼                                   ▼
                              ┌─────────────────┐               ┌──────────────────┐
                              │ OpenAI           │               │ Ollama            │
                              │ POST /v1/chat/   │               │ POST /api/chat    │
                              │ completions      │               │ stream: false     │
                              └─────────────────┘               └──────────────────┘
```

### Data Flow

```
Request Journey (happy path, cache miss):
─────────────────────────────────────────
  1. TCP accept → http.Server.Handler invokes logMiddleware
  2. logMiddleware: slog.Info("request", method, path) → next
  3. authMiddleware: bearerToken(r) lookup in apiKeys map[string]struct{}
     └─ if len(apiKeys)==0 → open (local dev)
     └─ if key missing    → 401 JSON {"error":{"message":"..."}}
  4. json.Decode → providers.Request{Model, Messages[], Temperature, MaxTokens, ...}
  5. Guardrails.Apply(&req) mutates req in place:
     ├─ For each user Message:
     │   ├─ matchInjection(content) → pattern scan over 5 compiled regexps
     │   │   └─ match → res.Blocked=true, return → handler writes 403
     │   └─ redact(content) → replace PII spans, res.Redactions++
     └─ returns Result{Blocked, Reason, Redactions}
  6. key := cache.Key(req)  // SHA-256(json.Marshal(req))
     ├─ memCache.Get(key): RLock → lookup → check expiry → RUnlock
     └─ HIT: w.Header.Set("X-LLMGate-Cache","hit"), writeJSON(resp) → done
  7. Router.Route(ctx, req):
     for each Provider in chain:
       for attempt := 1..MaxAttempts:
         resp, err := p.Chat(ctx, req)
         if err==nil → return resp
         slog.Warn("provider call failed", provider, attempt, err)
         if attempt < MaxAttempts: select { <-ctx.Done | <-time.After(Backoff) }
     all fail → fmt.Errorf("all providers failed: %w", errors.Join(errs...))
  8. Meter.Record(Record{Time, APIKey, Provider, Model, Usage, CostUSD})
     └─ memMeter: mu.Lock → tokens+=, cost+=, calls++ → mu.Unlock
     └─ slog.Info("metered", provider, model, total_tokens, cost_usd)
  9. Cache.Set(key, resp, 5*time.Minute)
     └─ memCache: mu.Lock → m[key]={resp, now+TTL} → mu.Unlock
 10. w.Header.Set("X-LLMGate-Cache","miss"), writeJSON(resp) → 200 OK
```

---

## Module Breakdown

### `internal/config` — Zero-Dependency YAML Parser

Config loading uses **no external YAML library**. The parser is a hand-rolled line scanner that reads a deliberate subset of YAML:

```go
// Config is the top-level gateway configuration.
type Config struct {
    Addr       string
    Providers  []Provider
    Timeout    time.Duration  // per upstream call; default 60s
    Retry      Retry
    Guardrails Guardrails
    Database   string         // DSN seam for metering.NewPostgres
    Redis      string         // addr seam for cache.NewRedis
    APIKeys    []string
}
```

**Priority chain** (later overrides earlier):
1. `config.Default()` — ships working against local Ollama `:11434` with no config file.
2. YAML file at `LLMGATE_CONFIG` — providers list, timeouts, guardrail toggles.
3. Environment variables — `LLMGATE_ADDR`, `LLMGATE_UPSTREAM_TIMEOUT_S`, `LLMGATE_API_KEYS`, `DATABASE_URL`, `REDIS_ADDR`.
4. `${ENV_VAR}` expansion inside YAML values — provider API keys never stored in files.

Validation asserts: at least one provider, every provider has a name and model.

---

### `internal/providers` — Provider Interface & Wire Format Adapters

The key design decision: **one internal request type, N external wire formats.** The `Provider` interface is the seam that lets the router not care which backend it's talking to:

```go
type Provider interface {
    Name() string
    Chat(ctx context.Context, req Request) (*Response, error)
}
```

`Request` is the gateway's canonical internal type — an intentional superset of both backends:

```go
type Request struct {
    Model          string          `json:"model"`
    Messages       []Message       `json:"messages"`
    Temperature    float64         `json:"temperature,omitempty"`
    MaxTokens      int             `json:"max_tokens,omitempty"`
    ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
    Think          *bool           `json:"think,omitempty"`  // nil = don't send
}
```

#### OpenAI → `POST /v1/chat/completions`

The OpenAI adapter performs field-level sanitization before serialization — `max_tokens: 0` is silently dropped because OpenAI rejects it:

```go
payload := map[string]any{
    "model":       model,
    "messages":    req.Messages,
    "temperature": req.Temperature,
}
if req.MaxTokens > 0 {
    payload["max_tokens"] = req.MaxTokens  // only if set
}
if req.ResponseFormat != nil {
    payload["response_format"] = req.ResponseFormat
}
```

Response parsing extracts `choices[0].message.content` and maps the OpenAI `usage` struct directly to the normalized `Usage` type.

#### Ollama → `POST /api/chat` (Wire Format Translation Table)

Ollama uses a structurally different API. The adapter translates every field:

| LLMGate `Request` field | OpenAI wire field | Ollama wire field | Notes |
|:---|:---|:---|:---|
| `Temperature` | `temperature` (top-level) | `options.temperature` | Ollama nests sampling under `options{}` |
| `MaxTokens` | `max_tokens` (top-level) | `options.num_predict` | Different field name |
| `ResponseFormat.Type == "json_object"` | `response_format: {"type":"json_object"}` | `format: "json"` | Different key and value |
| `Think` | Not supported | `think: bool` | Ollama-only reasoning toggle |
| *(implicit)* | *(absent)* | `stream: false` | Ollama defaults to streaming; must explicitly disable |

Usage is reconstructed from Ollama's `prompt_eval_count` and `eval_count` fields:

```go
Usage: Usage{
    PromptTokens:     parsed.PromptEvalCount,
    CompletionTokens: parsed.EvalCount,
    TotalTokens:      parsed.PromptEvalCount + parsed.EvalCount,
},
```

**Adding a new provider** is exactly two steps: implement the `Provider` interface, add a `case "name":` in `providers.Build()`.

---

### `internal/router` — Ordered Failover with Retry

The router holds an ordered `[]providers.Provider` slice and a `config.Retry` (max attempts + fixed backoff). The algorithm is a simple nested loop — deliberately not recursive, no goroutines, no complex state machine:

```
Route(ctx, req):
  for p in chain:                         // try each provider
    for attempt 1..MaxAttempts:           // retry this provider
      resp, err = p.Chat(ctx, req)
      if err == nil → return resp         // success, short-circuit everything
      append err to errs
      warn log
      if attempt < MaxAttempts:
        select { ctx.Done → return ctx err | time.After(Backoff) → continue }
  return nil, errors.Join(errs...)        // all providers exhausted
```

**Failure mode taxonomy:**

| Error type | Router behaviour |
|:---|:---|
| Provider HTTP 4xx (client error) | Counted as failure; retries & failover proceed |
| Provider HTTP 5xx / timeout | Same; `ctx.Err()` check short-circuits on deadline |
| Context cancelled by client | `ctx.Err()` returned immediately, no more attempts |
| All providers fail all attempts | Joined error returned; handler writes `502 Bad Gateway` |

**No jitter on backoff** — intentional for a reference. A production fork should add `rand.Int63n(backoff)` to the sleep to avoid thundering-herd on shared providers.

---

### `internal/cache` — Content-Addressed TTL Cache

Cache correctness requires that two structurally-identical requests produce the same key regardless of JSON field ordering. LLMGate achieves this by serializing the full `providers.Request` struct (which has deterministic field ordering via struct layout) through `json.Marshal`, then hashing:

```go
func Key(req providers.Request) string {
    b, _ := json.Marshal(req)   // deterministic: struct fields, not map
    sum := sha256.Sum256(b)
    return hex.EncodeToString(sum[:])
}
```

The in-memory implementation is a `map[string]entry` behind a `sync.RWMutex`. Reads use `RLock` (multiple concurrent readers); writes use `Lock`. Expiry is lazy — checked on read, not via a background sweeper:

```go
func (c *memCache) Get(key string) (*providers.Response, bool) {
    c.mu.RLock()
    e, ok := c.m[key]
    c.mu.RUnlock()
    if !ok || time.Now().After(e.expires) {
        return nil, false  // lazy expiry; stale entry stays in map
    }
    return e.resp, true
}
```

**Scaling path:** The `Cache` interface has exactly two methods (`Get`, `Set`). Swapping in Redis requires implementing those two methods and updating the wiring in `cmd/llmgate/main.go`. The DSN seam is already wired from config: `cache.NewRedis(cfg.Redis)` is a one-line change once the driver is added.

**Cache response header:** `X-LLMGate-Cache: hit` or `miss` lets clients and Grafana dashboards distinguish cache-served responses from upstream calls.

---

### Security Layer

<div align="center">

![LLMGate 3D Eval Gate](assets/llmgate-eval-gate-3d.jpg)

</div>

#### Prompt-Injection Detection (`internal/guardrails`)

Five compiled `*regexp.Regexp` patterns scan every `user` role message. The patterns are case-insensitive and catch the most common override, persona, and exfiltration attack families:

| Pattern | Attack class |
|:---|:---|
| `ignore (all\|any\|the) (previous\|prior\|above) (instructions\|prompts)` | System-prompt override |
| `disregard (your\|the) (system\|previous) (prompt\|instructions)` | System-prompt override |
| `you are now (in )?(developer\|dan\|jailbreak) mode` | Persona exploit |
| `reveal (your\|the) (system\|hidden) (prompt\|instructions)` | Prompt exfiltration |
| `print (your\|the) (system\|initial) prompt` | Prompt exfiltration |

On match: `Guardrails.Apply()` returns `Result{Blocked: true}` immediately — the request is **never serialized to the provider**. The handler writes `403 Forbidden` with a structured JSON error. Provider costs are zero.

Roadmap: replace heuristics with a classifier model invoked through the gateway itself (self-referential — the gateway defends via a model it routes to).

#### PII Redaction

Six regex patterns match PII in `content` strings of all message roles. Replacement is done in-place on the `req.Messages` slice before any network egress:

```go
var piiPatterns = map[string]*regexp.Regexp{
    "EMAIL":     regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
    "CARD":      regexp.MustCompile(`\b(?:\d[ -]*?){13,16}\b`),
    "SSN":       regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
    "IP":        regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`),
    "PHONE_IND": regexp.MustCompile(`\b(?:\+91[\-\s]?)?[6-9]\d{9}\b`),
}
```

Replacement token format: `[REDACTED_<LABEL>]` — parseable by downstream log analysis pipelines. `Result.Redactions` counts the total number of spans replaced across all messages in a single call.

**Before / After:**
```
Input:  "Ship order #4532-1234-5678-9010 to john@example.com from 192.168.1.1"
Output: "Ship order #[REDACTED_CARD] to [REDACTED_EMAIL] from [REDACTED_IP]"
```

---

### `internal/metering` — Token & Cost Accounting

Every successful upstream call records a `metering.Record` synchronously on the response path:

```go
type Record struct {
    Time     time.Time
    APIKey   string      // masked to last 4 chars in logs
    Provider string
    Model    string
    Usage    providers.Usage   // PromptTokens, CompletionTokens, TotalTokens
    CostUSD  float64
}
```

**Cost estimation** uses a static price table keyed by model name (USD per 1K total tokens). Unknown models cost `0` — the field is present but zero, not absent:

```go
var pricePer1K = map[string]float64{
    "gpt-4o-mini": 0.00075,
    "gpt-4o":      0.0125,
    "llama3.2":    0,      // local inference, zero API cost
}

func Cost(model string, u providers.Usage) float64 {
    return pricePer1K[model] * float64(u.TotalTokens) / 1000.0
}
```

The `memMeter` accumulates counters under a `sync.Mutex` (not `sync.RWMutex` — writes dominate in high-frequency metering). `Totals()` is used only for `/status` and `/metrics` reads, which are infrequent.

**Persistent metering seam:** `metering.NewPostgres(dsn string) Meter` already exists in the codebase. It returns `New()` (in-memory) until you add `pgx/v5` and implement the `INSERT` / `SUM` logic. The wiring in `cmd/llmgate/main.go` calls it with the config DSN — no structural change needed.

---

### The Eval Gate

<div align="center">

*The quality gate is not a test of infrastructure — it's a test of model output. It runs on every push and fails the build when answers regress or latency exceeds budget.*

</div>

#### Architecture of `internal/eval`

The harness defines a minimal interface that decouples evaluation from implementation:

```go
// ChatFunc is satisfied by: the live router, an HTTP client against a running
// gateway, or a deterministic stub in tests.
type ChatFunc func(ctx context.Context, req providers.Request) (*providers.Response, error)

type Case struct {
    Name           string              `json:"name"`
    Messages       []providers.Message `json:"messages"`
    ExpectContains []string            `json:"expect_contains"`
}

type Report struct {
    Results    []CaseResult
    Passed     int
    Total      int
    MaxLatency time.Duration
}
```

`score()` uses **case-insensitive substring matching** — not exact equality. This tolerates model verbosity differences while still catching factual regressions:

```go
func score(answer string, expect []string) bool {
    low := strings.ToLower(answer)
    for _, e := range expect {
        if !strings.Contains(low, strings.ToLower(e)) {
            return false   // short-circuit on first missing substring
        }
    }
    return true
}
```

#### CI Execution (`.github/workflows/eval.yml`)

```yaml
- name: Eval gate
  run: go test ./internal/eval/... -run TestGoldenSet -v -count=1
```

The test uses a `stubChat` function that deterministically answers the golden cases **without any network call**. This means CI never needs a live model — it validates the eval *harness* itself, plus the threshold logic:

```go
const (
    minPassRate = 1.0            // 100% required
    maxLatency  = 2 * time.Second
)
```

If either threshold is violated, `t.Errorf` is called and CI fails — blocking the merge.

#### Live Gateway Eval (`cmd/eval`)

The standalone CLI runs the same `golden.json` against a real running gateway over HTTP. It accepts flags:

```
-url         string   gateway endpoint (default "http://localhost:8080/v1/chat")
-api-key     string   bearer token ($LLMGATE_API_KEY)
-cases       string   golden set JSON path
-min-pass    float    minimum pass rate [0-1] (default 1.0)
-max-latency duration per-case latency budget (default 10s)
```

Output format:
```
LLMGate eval — 3 cases
-----------------------------------
  [PASS] capital-france            412ms
  [PASS] arithmetic                189ms
  [PASS] json-keyword              295ms
-----------------------------------
pass rate 100%  max latency 412ms

OK
```

**Extending the golden set:** append a JSON object to `internal/eval/testdata/golden.json`. Add a corresponding case in `stubChat` in `eval_test.go` to keep the unit test deterministic:

```json
{
  "name": "go-channels",
  "messages": [{"role": "user", "content": "In one sentence, what is a Go channel?"}],
  "expect_contains": ["goroutine", "communicate"]
}
```

---

### Empirical Research: The Winner's Curse Experiment

`experiments/winners-curse/` documents a real multi-week research run through LLMGate that surfaced both a statistical finding and three gateway bugs in production:

**Setup:** qwen3:4b (reasoning off) proposes BM25 retrieval configurations through LLMGate. 60 candidate configs. 100-query locked search split, 1,009 held-out queries. Metric: Recall@10 on BEIR SciFact.

**Key results:**

```
                              Search split    Held-out
  Baseline BM25               0.8050          0.7881
  LLM-picked winner           0.8500          0.8028   ← claimed +4.5, real +1.5
  Oracle (true best)          0.8300          0.8167   ← the search picked the wrong config
```

```
  Claimed gain  ████████████████████ +4.5 pts
  Real gain     ██████               +1.5 pts
  Inflation     ░░░░░░░░░░░░░░       +3.0 pts  (selection noise)
```

Across 1,000 random configs and 200 random splits: with a **25-query eval**, the best-of-999 claimed +8.3 and delivered −0.5. With **400 queries**, false-win rate drops to 2%.

**Gateway bugs surfaced during the experiment run:**

| Bug | Symptom | Fix |
|:---|:---|:---|
| Hard-coded 60s timeout | Reasoning model proposals took >60s, gateway hung | Made `upstream_timeout_s` configurable via config + env |
| Silently dropped `temperature`/`max_tokens` | Ollama ignored sampling parameters | Fixed Ollama adapter to send under `options{}` |
| No JSON mode | qwen3 returned prose instead of structured config JSON | Added `format: "json"` translation in Ollama adapter |

After fixes: proposal latency dropped from >2 minutes (with failed calls) to ~7 seconds. Zero gateway errors across 236 calls in the final sweep.

**Why this matters:** the experiment validated LLMGate under adversarial real-world load and directly drove three correctness fixes before the project was public.

---

## Quickstart

### Local (Ollama, zero cost)

```bash
ollama pull llama3.2
go run ./cmd/llmgate          # :8080 → local Ollama
```

```bash
curl -s localhost:8080/v1/chat -d '{
  "messages": [{"role":"user","content":"What is the capital of France?"}]
}' | jq
```

### With OpenAI + failover

```bash
cp config.example.yaml config.yaml
export OPENAI_API_KEY=sk-...
LLMGATE_CONFIG=config.yaml go run ./cmd/llmgate
```

### Full stack (gateway + Postgres + Redis)

```bash
docker compose up --build
```

Services: `llmgate:8080`, `postgres:5432`, `redis:6379`. PostgreSQL and Redis containers are present and wired; driver implementations are the only remaining step.

---

## Configuration

Config file (`config.yaml`) with environment variable expansion. All fields have safe defaults — zero-config runs against local Ollama.

```yaml
addr: ":8080"

providers:
  - name: openai              # first in chain = primary
    base_url: https://api.openai.com
    api_key: ${OPENAI_API_KEY}
    model: gpt-4o-mini
  - name: ollama              # fallback; no api_key needed
    base_url: http://localhost:11434
    model: llama3.2

upstream_timeout_s: 60        # per upstream HTTP call
max_attempts: 2               # retries per provider before failover
backoff_ms: 200               # fixed sleep between retries

block_prompt_injection: true  # 403 on heuristic injection patterns
redact_pii: true              # in-place regex redaction before provider

api_keys: ""                  # comma-separated; empty = open (local dev only)

# database: postgres://llmgate:llmgate@localhost:5432/llmgate?sslmode=disable
# redis: localhost:6379
```

**Environment overrides** (useful for container deployments):

| Variable | Field |
|:---|:---|
| `LLMGATE_ADDR` | `addr` |
| `LLMGATE_UPSTREAM_TIMEOUT_S` | `upstream_timeout_s` |
| `LLMGATE_API_KEYS` | `api_keys` (comma-separated) |
| `DATABASE_URL` | `database` |
| `REDIS_ADDR` | `redis` |
| `OPENAI_API_KEY` | expanded inside provider `api_key: ${OPENAI_API_KEY}` |

---

## API Reference

### Endpoints

| Method | Path | Auth | Description |
|:---|:---|:---:|:---|
| `POST` | `/v1/chat` | optional | Chat completion (OpenAI-compatible body) |
| `GET` | `/healthz` | — | Liveness probe → `{"status":"ok"}` |
| `GET` | `/status` | — | JSON: calls, tokens, cost, guardrail config |
| `GET` | `/metrics` | — | Prometheus text exposition (no exporter needed) |

### Request

```json
{
  "model": "gpt-4o-mini",
  "messages": [
    {"role": "system", "content": "You are a concise assistant."},
    {"role": "user",   "content": "Explain goroutines in one sentence."}
  ],
  "temperature": 0.2,
  "max_tokens": 128,
  "response_format": {"type": "json_object"},
  "think": false
}
```

| Field | Type | Required | Notes |
|:---|:---|:---:|:---|
| `messages` | array | ✓ | `role` + `content`; forwarded verbatim after PII redaction |
| `model` | string | | Falls back to provider config default if omitted |
| `temperature` | float | | Forwarded; Ollama: inside `options{}` |
| `max_tokens` | int | | OpenAI: `max_tokens`, Ollama: `options.num_predict` |
| `response_format` | object | | `{"type":"json_object"}` → Ollama: `format:"json"` |
| `think` | bool | | Ollama-only reasoning toggle; `nil` = omitted from payload |

### Response

```json
{
  "content":  "Goroutines are lightweight threads managed by the Go runtime.",
  "model":    "gpt-4o-mini",
  "provider": "openai",
  "usage": {
    "prompt_tokens":     24,
    "completion_tokens": 12,
    "total_tokens":      36
  }
}
```

**Response headers:** `X-LLMGate-Cache: hit | miss`, `Content-Type: application/json`

### Metrics (`GET /metrics`)

Hand-rolled Prometheus text format (no `client_golang` dependency):

```
# HELP llmgate_calls_total Total chat calls served.
# TYPE llmgate_calls_total counter
llmgate_calls_total 127

# HELP llmgate_tokens_total Total tokens across all calls.
# TYPE llmgate_tokens_total counter
llmgate_tokens_total 9450

# HELP llmgate_cost_usd_total Estimated spend in USD.
# TYPE llmgate_cost_usd_total counter
llmgate_cost_usd_total 0.007088
```

---

## Deployment

### Docker (distroless, non-root)

The Dockerfile is a two-stage build. The runtime image is `gcr.io/distroless/static-debian12` — no shell, no package manager, no libc. The binary runs as `nonroot:nonroot`:

```dockerfile
FROM golang:1.26-alpine AS build
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/llmgate ./cmd/llmgate

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/llmgate /llmgate
USER nonroot:nonroot
ENTRYPOINT ["/llmgate"]
```

`-ldflags="-s -w"` strips the symbol table and DWARF debug info, shrinking the binary by ~30%.

### Kubernetes (minimal)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: llmgate
spec:
  replicas: 2
  template:
    spec:
      containers:
      - name: llmgate
        image: ghcr.io/harshakumar25/llmgate:latest
        ports:
        - containerPort: 8080
        env:
        - name: OPENAI_API_KEY
          valueFrom:
            secretKeyRef: {name: llmgate-secrets, key: openai-api-key}
        - name: LLMGATE_API_KEYS
          valueFrom:
            secretKeyRef: {name: llmgate-secrets, key: gateway-api-keys}
        livenessProbe:
          httpGet: {path: /healthz, port: 8080}
        readinessProbe:
          httpGet: {path: /healthz, port: 8080}
        resources:
          requests: {memory: "25Mi", cpu: "50m"}
          limits:   {memory: "64Mi", cpu: "500m"}
```

---

## Development

```bash
make run        # go run ./cmd/llmgate (LLMGATE_CONFIG=config.yaml if present)
make test       # go test ./... -count=1
make eval       # go run ./cmd/eval -url http://localhost:8080/v1/chat
make vet        # go vet ./...
make build      # go build -o bin/llmgate ./cmd/llmgate
make docker     # docker build -t llmgate:dev .
make up         # docker compose up --build
make down       # docker compose down
make tidy       # go mod tidy
```

### Repository Layout

```
cmd/
  llmgate/        main.go — wires config, providers, router, cache, meter, server
  eval/           main.go — standalone CLI eval runner against live gateway
internal/
  config/         config.go — zero-dep YAML parser + env override
  server/         server.go — HTTP mux, auth middleware, log middleware
                  handlers.go — handleChat, handleHealth, handleMetrics, handleStatus
  providers/      provider.go — Provider interface, Request/Response types, Build()
                  openai.go — OpenAI adapter (POST /v1/chat/completions)
                  ollama.go — Ollama adapter (POST /api/chat, format translation)
  router/         router.go — ordered failover with retry + backoff
  guardrails/     guardrails.go — injection scanner + PII redactor
  cache/          cache.go — SHA-256 keyed TTL cache, memCache + NewRedis seam
  metering/       metering.go — Meter interface, memMeter, Cost(), NewPostgres seam
  eval/           eval.go — Run(), LoadCases(), Report, ChatFunc type
                  eval_test.go — TestGoldenSet CI quality gate + stubChat
                  testdata/golden.json — golden evaluation cases
experiments/
  winners-curse/  README.md — empirical retrieval research
                  experiment.py — 1,000-config sweep, BM25 search via LLMGate
assets/           3D architecture visualizations
Dockerfile        Multi-stage, distroless, non-root
docker-compose.yml gateway + postgres:16-alpine + redis:7-alpine
go.mod            module github.com/harshakumar25/llmgate — zero dependencies
Makefile          Developer workflow targets
```

---

## Roadmap

| Status | Item | Seam in code |
|:---:|:---|:---|
| ✅ | OpenAI & Ollama provider adapters | `internal/providers/` |
| ✅ | Ordered failover + configurable retry/backoff | `internal/router/router.go` |
| ✅ | SHA-256 content-addressed in-memory cache | `internal/cache/cache.go` |
| ✅ | Heuristic prompt-injection scanner (403) | `internal/guardrails/guardrails.go` |
| ✅ | In-place PII redaction (5 entity types) | `internal/guardrails/guardrails.go` |
| ✅ | Per-call token + USD metering (in-memory) | `internal/metering/metering.go` |
| ✅ | Prometheus text metrics at `/metrics` | `internal/server/handlers.go` |
| ✅ | Golden-set CI eval gate | `internal/eval/` + `.github/workflows/eval.yml` |
| ✅ | Distroless Docker container | `Dockerfile` |
| ⬜ | SSE streaming passthrough | `handleChat` → `http.Flusher` |
| ⬜ | PostgreSQL metering backend | `metering.NewPostgres` stub exists |
| ⬜ | Redis cache backend | `cache.NewRedis` stub exists |
| ⬜ | Semantic / embedding cache | needs embedding provider |
| ⬜ | Anthropic & Gemini adapters | add cases in `providers.Build()` |
| ⬜ | Classifier-based injection detector | replace heuristic in `guardrails.go` |
| ⬜ | Per-key rate limiting | middleware layer in `server.go` |
| ⬜ | Output-side safety scanning | post-router stage in `handleChat` |

---

## License

MIT © Harsh Kumar
