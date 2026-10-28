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
| Multiple providers, multiple wire formats | Normalized `Provider` interface; OpenAI, Ollama and Anthropic adapters | [`internal/providers`](internal/providers/) |
| Chat UIs need tokens as they are generated | SSE streaming from all three providers, one stream format for clients | [`internal/providers/stream.go`](internal/providers/stream.go) |
| Provider outages & transient errors | Ordered failover, error-aware retry with jittered backoff, per-provider circuit breaker | [`internal/router`](internal/router/) |
| Repeated identical requests wasting tokens | SHA-256 content-addressed, size-bounded LRU cache with TTL; optional Redis tier shared across replicas | [`internal/cache`](internal/cache/) |
| Bursts of the same prompt hitting the model at once | Coalesces identical in-flight requests into one upstream call | [`internal/flight`](internal/flight/) |
| Prompt injection & data exfiltration | Pre-flight heuristic regex scanner, returns `403` before hitting provider | [`internal/guardrails`](internal/guardrails/) |
| PII leaking into cloud APIs | In-place regex redaction of email, card, SSN, IP, phone; optional redaction of replies too | [`internal/guardrails`](internal/guardrails/) |
| One client exhausting the budget | Per-key token-bucket rate limit, `429` + `Retry-After` | [`internal/ratelimit`](internal/ratelimit/) |
| No visibility into token spend | Per-call metering with USD estimate; Prometheus `/metrics` | [`internal/metering`](internal/metering/) |
| Model regressions invisible until production | Golden-set eval harness in CI; fails build on quality drop | [`internal/eval`](internal/eval/) |

> Most AI teams build a worse version of this in-house. LLMGate is a clean reference you can self-host, read in an afternoon, and extend.

---

## Architecture

<div align="center">

![LLMGate 3D Architecture](assets/llmgate-architecture-3d.jpg)

*Every inbound request passes through Auth → Guardrails → Cache → Coalescing → Router. Metering intercepts the response path, once per upstream call. The shape is a strict pipeline: no goroutines per hop; the only state shared between requests is the cache, the in-flight table and the router's breaker counters, each behind its own mutex.*

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
                        │   ④ flight.Do(key) ─── joins an identical in-flight call     │
                        │      Router.Route(ctx, req)                                  │
                        │      ├─ breaker open? ── skip provider                       │
                        │      ├─ Provider[0].Chat() ── retry (5xx/429/timeout only)   │
                        │      └─ Provider[N].Chat() ── failover                       │
                        │   ⑤ Meter.Record(usage, cost) ── once per upstream call      │
                        │   ⑥ Cache.Set(key, resp, TTL) ── LRU, bounded                │
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
  2. logMiddleware: wraps ResponseWriter, calls next, then
     slog.Info("request", method, path, status, cache, ms)
  3. authMiddleware: bearerToken(r) lookup in apiKeys map[string]struct{}
     └─ if len(apiKeys)==0 → open (local dev)
     └─ if key missing    → 401 JSON {"error":{"message":"..."}}
  4. json.Decode(MaxBytesReader(1 MiB)) → providers.Request{Model, Messages[], ...}
     └─ body too large → 413
  5. Guardrails.Apply(&req) mutates req in place:
     ├─ For each user Message:
     │   ├─ matchInjection(content) → pattern scan over 5 compiled regexps
     │   │   └─ match → res.Blocked=true, return → handler writes 403
     │   └─ redact(content) → replace PII spans, res.Redactions++
     └─ returns Result{Blocked, Reason, Redactions}
  6. key := cache.Key(req)  // SHA-256(json.Marshal(req))
     ├─ skipped when cache_ttl_s is 0 or the client sends Cache-Control: no-cache
     ├─ memCache.Get(key): Lock → lookup → expired? drop : MoveToFront → Unlock
     └─ HIT: w.Header.Set("X-LLMGate-Cache","hit"), writeJSON(resp) → done
  7. flight.Do(key, upstream):
     ├─ an identical call is in flight → wait for it, share its result
     │   └─ X-LLMGate-Cache: coalesced
     └─ otherwise run upstream() with context.WithoutCancel(r.Context()),
        so one client hanging up does not fail the others waiting on it
  8. Router.Route(ctx, req):
     for each Provider in chain:
       breaker open and cooling down → skip (counted in skipped_total)
       for attempt := 1..MaxAttempts:
         resp, err := p.Chat(ctx, req)
         if err==nil → reset failure streak, return resp
         if !Retryable(err) → fail over now (400/401/404 cannot succeed)
         sleep jittered backoff: d = min(base·2^(n-1), cap), wait ∈ [d/2, d)
     no provider admitted → ErrAllOpen → 503 + Retry-After
     all fail → fmt.Errorf("all providers failed: %w", errors.Join(errs...))
  9. Meter.Record(Record{Time, APIKey, Provider, Model, Usage, CostUSD})
     └─ memMeter: mu.Lock → tokens+=, cost+=, calls++ → mu.Unlock
     └─ slog.Info("metered", provider, model, total_tokens, cost_usd)
 10. Cache.Set(key, resp, TTL)
     └─ memCache: Lock → full? evict expired-near-tail else LRU tail → PushFront → Unlock
 11. w.Header.Set("X-LLMGate-Cache","miss"), writeJSON(resp) → 200 OK
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
3. Environment variables — `LLMGATE_ADDR`, `LLMGATE_UPSTREAM_TIMEOUT_S`, `LLMGATE_API_KEYS`, `LLMGATE_RATE_LIMIT_RPM`, `DATABASE_URL`, `REDIS_ADDR`.
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

// Optional: providers that can stream implement this too.
type Streamer interface {
    ChatStream(ctx context.Context, req Request, onDelta func(string) error) (*Response, error)
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
    Stream         bool            `json:"stream,omitempty"` // transport only; not part of the cache key
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

| LLMGate `Request` field | OpenAI wire field | Ollama wire field | Anthropic wire field | Notes |
|:---|:---|:---|:---|:---|
| `system` messages | in `messages` | in `messages` | top-level `system` string | Anthropic lifts system turns out of the list |
| `Temperature` | `temperature` (top-level) | `options.temperature` | `temperature` | Ollama nests sampling under `options{}` |
| `MaxTokens` | `max_tokens` (top-level) | `options.num_predict` | `max_tokens` (**required**, default 1024) | Different names, different rules |
| `ResponseFormat.Type == "json_object"` | `response_format: {"type":"json_object"}` | `format: "json"` | not sent | Different key and value |
| `Think` | Not supported | `think: bool` | not sent | Ollama-only reasoning toggle |
| `Stream` | `stream: true` + `stream_options.include_usage` | `stream: true` | `stream: true` | Ollama defaults to streaming; `Chat` sends `stream: false` |
| auth | `Authorization: Bearer` | none | `x-api-key` + `anthropic-version: 2023-06-01` | |

Usage is reconstructed from Ollama's `prompt_eval_count` and `eval_count` fields:

```go
Usage: Usage{
    PromptTokens:     parsed.PromptEvalCount,
    CompletionTokens: parsed.EvalCount,
    TotalTokens:      parsed.PromptEvalCount + parsed.EvalCount,
},
```

Non-2xx replies from every adapter come back as a typed `*providers.StatusError{Provider, Code, Body}`, which the router uses to decide whether a retry can help.

#### Streaming wire formats

Each provider streams differently; the adapters turn all three into the same `onDelta(text)` callback plus a final `Response` with exact usage:

| Provider | Framing | Text lives in | Token counts arrive in |
|:---|:---|:---|:---|
| OpenAI | SSE `data:` lines, ends with `data: [DONE]` | `choices[0].delta.content` | last chunk's `usage` (requested with `include_usage`) |
| Ollama | newline-delimited JSON objects | `message.content` | final object with `done: true` (`prompt_eval_count`, `eval_count`) |
| Anthropic | SSE `event:` + `data:` pairs | `content_block_delta` → `delta.text` | `message_start` (input) and `message_delta` (output) |

One scanner (`scanLines`, `scanSSE` in `stream.go`) handles framing for all three, with a 1 MiB line limit for large events.

**Adding a new provider** is two steps: implement the `Provider` interface (and `Streamer` if it can stream), and add a `case "name":` in `providers.Build()`. A provider without `Streamer` still works for streaming clients: the router calls `Chat` and delivers the reply as one fragment.

---

### `internal/router` — Failover, Error-Aware Retry, Circuit Breaker

The router walks an ordered provider chain. Each provider carries its own breaker state and counters. The loop is still a plain nested loop with no goroutines; the decisions inside it are what changed:

```
Route(ctx, req):
  for p in chain:
    if !admit(p): continue                  // breaker open → skip, no network call
    for attempt 1..MaxAttempts:
      resp, err = p.Chat(ctx, req)
      record(p, latency, err)               // counters + breaker bookkeeping
      if err == nil → return resp
      if caller gone → return ctx.Err()     // never retry for a client that left
      if !Retryable(err) → break            // fail over immediately
      sleep(backoff(attempt))               // jittered, capped
  if no provider was admitted → ErrAllOpen  // handler: 503 + Retry-After
  return errors.Join(errs...)               // handler: 502
```

**Failure mode taxonomy:**

| Error | Retry same provider? | Counts against breaker? | Result |
|:---|:---:|:---:|:---|
| `5xx`, `408`, `429`, transport error, upstream timeout | ✓ | ✓ | Retry with backoff, then fail over |
| Other `4xx` (bad request, bad key, unknown model) | ✗ | ✗ | Fail over at once; a malformed request says nothing about provider health |
| Caller cancelled / caller deadline | ✗ | ✗ | Return `ctx.Err()` immediately |
| Every provider's breaker open | — | — | `503` + `Retry-After: breaker_cooldown_s`, no upstream call |
| All admitted providers failed | — | — | `502 Bad Gateway` with the joined error |

Providers return a typed `*providers.StatusError{Code}` for non-2xx replies, and `providers.Retryable(err)` makes the call.

**Backoff.** `d = min(backoff_ms · 2^(attempt-1), max_backoff_ms)`, and the actual wait is drawn uniformly from `[d/2, d)`. Doubling backs off a struggling upstream; the jitter keeps many clients that failed together from retrying in lockstep.

**Circuit breaker.** After `breaker_failures` consecutive retryable failures a provider is *open* for `breaker_cooldown_s`: requests skip it without a network call. When the cooldown ends it is *half-open*: exactly one request is admitted as a trial while others keep skipping it. A successful trial closes the breaker; a failed one reopens it for another cooldown. Without this, a dead primary costs every request a full `upstream_timeout_s` before failover.

---

### `internal/cache` — Bounded LRU with TTL

Cache correctness requires that two structurally-identical requests produce the same key regardless of JSON field ordering. LLMGate serializes the full `providers.Request` struct (deterministic field order via struct layout) through `json.Marshal`, then hashes:

```go
func Key(req providers.Request) string {
    b, _ := json.Marshal(req)   // deterministic: struct fields, not map
    sum := sha256.Sum256(b)
    return hex.EncodeToString(sum[:])
}
```

The in-memory implementation is a `container/list` LRU plus a `map[string]*list.Element`, behind one mutex. It is **bounded** (`cache_max_entries`, default 10,000), so memory cannot grow without limit:

| Operation | Behaviour |
|:---|:---|
| `Get` hit | Move entry to the front (most recently used) |
| `Get` on expired entry | Delete it, count as `expired` + `miss` |
| `Set` when full | Look at the 8 least-recent entries; drop an expired one if present, else evict the LRU tail |
| `Stats()` | hits, misses, evictions, expired, entries → `/metrics` |

**Bypass.** `cache_ttl_s: 0` turns caching (and coalescing) off. A client can force a fresh call per request with `Cache-Control: no-cache`; the response then carries `X-LLMGate-Cache: bypass`.

#### Redis tier (`redis.go`, zero dependencies)

Set `redis: host:6379` (or `REDIS_ADDR`) and replicas share completions. The cache becomes two tiers: the local LRU above in front of Redis.

| Step | What happens |
|:---|:---|
| `Get` | Local LRU first. On a local miss, `GET` and `PTTL` go to Redis in **one pipelined round trip**; a hit is copied into the local tier with Redis's remaining TTL, so both copies expire together |
| `Set` | Write local, then `SET key value PX ttl_ms` |
| Keys | `llmgate:v1:<sha256>`; bump the version if the stored shape changes |
| Failure | **Fails open.** 250 ms deadline per call; on any error the connection is dropped, the lookup counts as a miss and the request goes to the provider. A Redis outage costs money, never availability. Counted in `llmgate_cache_remote_errors_total` |

The client speaks just enough of the Redis protocol (RESP: arrays of bulk strings out; simple, error, integer and bulk replies in) over a small connection pool, about 200 lines, so `go.mod` stays empty. `redis_test.go` runs it against an in-process fake Redis server, so CI needs no Redis.

---

### `internal/flight` — Request Coalescing

A cache only helps after the first answer lands. A burst of identical prompts that arrive *while the first is still running* (a retry storm, a fan-out, a popular question whose cache entry just expired) would all go upstream. `flight.Group` is a small generic take on `golang.org/x/sync/singleflight`:

```
flight.Do(key, fn):
  key already in flight → wait on its WaitGroup, return the same (resp, err), shared=true
  otherwise             → register key, run fn, release waiters, forget key (even on panic)
```

- The upstream call runs under `context.WithoutCancel(r.Context())`, so one waiting client hanging up does not fail the rest; `upstream_timeout_s` still bounds it.
- Metering happens inside `fn`, so tokens are counted **once per upstream call**, not once per waiting client.
- Errors are shared too, and the key is released, so the next request after a failure tries again.

**Measured:** in `TestBurstOfIdenticalRequestsMakesOneUpstreamCall`, 50 concurrent identical requests to a provider that takes 200 ms produce **1 upstream call** and 49 `coalesced` responses. Against a live `qwen3:4b` on Ollama, 20 concurrent identical requests made 1 model call, and the 21st was a cache hit.

---

### Streaming (SSE)

Send `"stream": true` and the reply arrives as server-sent events, in one format whatever the provider:

```
data: {"delta":"Hel"}
data: {"delta":"lo"}
data: {"done":true,"model":"qwen3:4b","provider":"ollama","usage":{"prompt_tokens":21,"completion_tokens":40,"total_tokens":61}}
data: [DONE]
```

```bash
curl -N localhost:8080/v1/chat -d '{"stream":true,"messages":[{"role":"user","content":"Count to 8"}]}'
```

**The commit point.** `Router.RouteStream` retries and fails over exactly like `Route` *until the first fragment reaches the client*. After that the client already holds partial output, so restarting on another provider would splice two different answers together. A failure past that point ends the stream with a final `data: {"error":{"message":"..."}}` event instead.

| Situation | Client sees |
|:---|:---|
| Upstream fails before any output | Ordinary JSON error with a real status (`502`, `503`), because headers are sent lazily on the first event |
| Upstream fails mid-stream | The fragments so far, then an `error` event |
| Client disconnects mid-stream | Upstream call is cancelled; not counted against the provider's breaker |
| Prompt is already cached | `X-LLMGate-Cache: hit`, the whole reply as one `delta`, then `done` |

**Caching and streams.** The cache key ignores `stream`, so a finished stream fills the cache and a later identical request, streamed or not, skips the provider (`TestStreamingProxiesDeltasThenServesFromCache`). Streams are not coalesced: each client wants its own live tokens.

Measured live against `qwen3:4b`: fragments arrived about 20 ms apart through the gateway, the `done` event carried exact token counts, and the repeat request was a cache hit with no model call. Responses also set `X-Accel-Buffering: no` so nginx-style proxies don't hold tokens back.

---

### `internal/ratelimit` — Per-Key Token Bucket

`rate_limit_rpm: 60` with `rate_limit_burst: 10` lets each key make 10 requests at once, then one per second on average. The bucket key is the API key, or the client IP when the gateway runs without keys. The limiter runs **after** auth, so junk requests with a bad key can't drain a real key's budget.

| Response header | Meaning |
|:---|:---|
| `X-RateLimit-Limit` | Bucket size (burst) |
| `X-RateLimit-Remaining` | Tokens left after this request |
| `Retry-After` | On `429`: seconds until one token refills |

Each active key costs one small struct, with no background goroutine. Refill is computed lazily from the time since the last request; once the table passes 10,000 keys, buckets that have refilled completely are dropped, which changes nothing for those keys. Rejections are counted in `llmgate_rate_limited_total`.

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

Five regex patterns match PII in `content` strings of all message roles. Replacement is done in-place on the `req.Messages` slice before any network egress. The patterns run in a **fixed order**:

```go
var piiPatterns = []struct {
    label string
    re    *regexp.Regexp
}{
    {"EMAIL", ...}, {"CARD", ...}, {"SSN", ...}, {"IP", ...}, {"PHONE_IND", ...},
}
```

The order matters because the patterns overlap: a 16-digit card number contains a valid 10-digit Indian mobile number. An earlier version kept them in a Go map, whose iteration order is random, so the same prompt could redact differently from one request to the next. That also gave it a different cache key each time. `TestRedactionIsDeterministic` pins this down.

**Output redaction.** With `redact_output: true` the same patterns also scrub the model's reply before it is returned and cached, for when a model echoes or invents personal data. It applies to non-streamed replies and to the cached copy of a stream; live stream fragments are passed through as they arrive, because a pattern can span two fragments.

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
  - name: anthropic           # second
    api_key: ${ANTHROPIC_API_KEY}
    model: claude-haiku-5-5
  - name: ollama              # local fallback; no api_key needed
    base_url: http://localhost:11434
    model: llama3.2

upstream_timeout_s: 60        # per upstream HTTP call
max_attempts: 2               # attempts per provider before failover
backoff_ms: 200               # base retry delay; doubles per attempt, jittered
max_backoff_ms: 2000          # cap on a single retry delay

breaker_failures: 5           # consecutive retryable failures that open a breaker (0 = off)
breaker_cooldown_s: 30        # how long an open provider is skipped

cache_max_entries: 10000      # LRU bound
cache_ttl_s: 300              # 0 disables caching and coalescing

block_prompt_injection: true  # 403 on heuristic injection patterns
redact_pii: true              # in-place regex redaction before provider
redact_output: false          # also scrub PII from replies

rate_limit_rpm: 0             # per key (or client IP); 0 = off
rate_limit_burst: 0           # 0 = same as rate_limit_rpm

api_keys: ""                  # comma-separated; empty = open (local dev only)

# redis: localhost:6379       # share the cache across replicas
# database: postgres://llmgate:llmgate@localhost:5432/llmgate?sslmode=disable
```

**Environment overrides** (useful for container deployments):

| Variable | Field |
|:---|:---|
| `LLMGATE_ADDR` | `addr` |
| `LLMGATE_UPSTREAM_TIMEOUT_S` | `upstream_timeout_s` |
| `LLMGATE_API_KEYS` | `api_keys` (comma-separated) |
| `LLMGATE_RATE_LIMIT_RPM` | `rate_limit_rpm` |
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
| `GET` | `/status` | — | JSON: calls, tokens, cost, cache + coalescing, per-provider calls/errors/avg latency/breaker |
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
| `stream` | bool | | Reply as server-sent events (see [Streaming](#streaming-sse)) |

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

**Response headers:** `X-LLMGate-Cache: hit | miss | coalesced | bypass`, `Content-Type: application/json` (or `text/event-stream` when streaming), `X-RateLimit-Limit` / `X-RateLimit-Remaining` when rate limiting is on, and `Retry-After` on `429` and `503`.

**Status codes:** `400` bad JSON or no messages · `401` bad API key · `403` blocked by guardrails · `413` body over 1 MiB · `429` rate limit · `502` all providers failed · `503` every provider's breaker is open.

### Metrics (`GET /metrics`)

Hand-rolled Prometheus text format (no `client_golang` dependency):

| Metric | Type | Meaning |
|:---|:---|:---|
| `llmgate_calls_total` | counter | Upstream chat calls that succeeded |
| `llmgate_tokens_total` | counter | Tokens across all calls |
| `llmgate_cost_usd_total` | counter | Estimated spend in USD |
| `llmgate_cache_hits_total` / `_misses_total` | counter | Cache lookups |
| `llmgate_cache_evictions_total` | counter | Entries dropped to stay under the bound |
| `llmgate_cache_entries` | gauge | Entries in the local cache tier |
| `llmgate_cache_remote_hits_total` | counter | Local misses answered by Redis |
| `llmgate_cache_remote_errors_total` | counter | Redis calls that failed open |
| `llmgate_rate_limited_total` | counter | Requests rejected with `429` |
| `llmgate_coalesced_total` | counter | Requests that shared an in-flight upstream call |
| `llmgate_upstream_flights_total` | counter | Coalescing groups that made an upstream call |
| `llmgate_provider_calls_total{provider}` | counter | Calls per provider |
| `llmgate_provider_errors_total{provider}` | counter | Failed calls per provider |
| `llmgate_provider_skipped_total{provider}` | counter | Requests that skipped an open breaker |
| `llmgate_provider_latency_seconds_total{provider}` | counter | Summed latency; divide by calls for the mean |
| `llmgate_provider_breaker_open{provider}` | gauge | `1` while the breaker is open |

```
llmgate_cache_hits_total 1
llmgate_coalesced_total 19
llmgate_upstream_flights_total 1
llmgate_provider_calls_total{provider="ollama"} 1
llmgate_provider_breaker_open{provider="ollama"} 0
```

*(From the live Ollama run above: 21 client requests, 1 model call.)*

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

Performance on an Apple M4 (`make bench`):

| Benchmark | Result |
|:---|:---|
| `BenchmarkHotPrompt`: full handler on a cached prompt (decode → guardrails → cache → encode), parallel | **~4.2 µs/op**, 80 allocs |
| `BenchmarkGetParallel`: cache lookup under contention | **~170 ns/op** |

```bash
make run        # go run ./cmd/llmgate (LLMGATE_CONFIG=config.yaml if present)
make test       # go test ./... -race -count=1
make bench      # gateway overhead + cache benchmarks
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
  server/         server.go — HTTP mux, auth middleware, status-aware log middleware
                  handlers.go — handleChat, handleHealth, handleMetrics, handleStatus
                  server_test.go — burst/coalescing, cache, 413, 503, metrics, benchmark
  providers/      provider.go — Provider interface, Request/Response types, Build()
                  openai.go — OpenAI adapter (POST /v1/chat/completions)
                  ollama.go — Ollama adapter (POST /api/chat, format translation)
                  anthropic.go — Anthropic adapter (POST /v1/messages)
                  stream.go — Streamer interface, SSE/NDJSON scanners, per-provider streams
  router/         router.go — failover, error-aware retry, jittered backoff, circuit breaker
  flight/         flight.go — coalescing of identical in-flight requests
  guardrails/     guardrails.go — injection scanner + PII redactor
  cache/          cache.go — SHA-256 keyed bounded LRU with TTL
                  redis.go — Redis tier over a built-in RESP client, fails open
  ratelimit/      ratelimit.go — per-key token bucket
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
| ✅ | OpenAI, Ollama & Anthropic provider adapters | `internal/providers/` |
| ✅ | SSE streaming from all providers, failover up to the first byte | `internal/providers/stream.go`, `RouteStream` |
| ✅ | Ordered failover, error-aware retry, jittered exponential backoff | `internal/router/router.go` |
| ✅ | Per-provider circuit breaker with half-open trial | `internal/router/router.go` |
| ✅ | SHA-256 content-addressed, bounded LRU cache | `internal/cache/cache.go` |
| ✅ | Redis cache tier (zero-dependency RESP client, fails open) | `internal/cache/redis.go` |
| ✅ | Per-key rate limiting (token bucket, `429`) | `internal/ratelimit/` |
| ✅ | Output-side PII redaction | `guardrails.RedactOutput` |
| ✅ | Coalescing of identical in-flight requests | `internal/flight/flight.go` |
| ✅ | Heuristic prompt-injection scanner (403) | `internal/guardrails/guardrails.go` |
| ✅ | In-place PII redaction (5 entity types) | `internal/guardrails/guardrails.go` |
| ✅ | Per-call token + USD metering (in-memory) | `internal/metering/metering.go` |
| ✅ | Prometheus text metrics at `/metrics` | `internal/server/handlers.go` |
| ✅ | Golden-set CI eval gate | `internal/eval/` + `.github/workflows/eval.yml` |
| ✅ | Distroless Docker container | `Dockerfile` |
| ⬜ | Cache only deterministic requests by default (`temperature: 0`) | `handleChat` cache policy |
| ⬜ | PostgreSQL metering backend | `metering.NewPostgres` stub exists |
| ⬜ | Semantic / embedding cache | needs embedding provider |
| ⬜ | Gemini adapter | add a case in `providers.Build()` |
| ⬜ | Output-side injection / policy scanning | post-router stage in `handleChat` |
| ⬜ | Classifier-based injection detector | replace heuristic in `guardrails.go` |

---

## License

MIT © Harsh Kumar
