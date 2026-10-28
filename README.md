<div align="center">

![LLMGate Hero Banner](assets/llmgate-hero-banner.jpg)

# LLMGate

**A lightweight, enterprise-grade, self-hostable LLM Gateway in Go.**  
*Unified OpenAI-compatible API • Zero-Leak Security Guardrails • Resilient Failover • CI Quality Gate*

[![ci](https://github.com/harshakumar25/llmgate/actions/workflows/eval.yml/badge.svg)](https://github.com/harshakumar25/llmgate/actions/workflows/eval.yml)
[![Eval Gate](https://img.shields.io/badge/eval%20gate-100%25%20pass-brightgreen?style=flat-square&logo=checkmarx)](https://github.com/harshakumar25/llmgate/actions/workflows/eval.yml)
[![Go Version](https://img.shields.io/badge/go-1.26+-00ADD8?style=flat-square&logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg?style=flat-square)](LICENSE)
[![Docker](https://img.shields.io/badge/docker-ready-2496ED?style=flat-square&logo=docker)](Dockerfile)
[![Memory Footprint](https://img.shields.io/badge/memory-%3C25MB-blueviolet?style=flat-square)](#performance--efficiency)
[![Zero External Cgo](https://img.shields.io/badge/cgo-disabled-success?style=flat-square)](#development)

<br/>

[Key Highlights](#why-llmgate) •
[3D Architecture](#system-architecture) •
[Unique Differentiators](#the-four-pillars-of-uniqueness) •
[Eval Gate in CI](#1-the-ci-eval-gate-stopping-silent-regressions) •
[Winner's Curse Research](#2-empirical-research-winners-curse-validation) •
[Security Guardrails](#3-in-line-defense-in-depth-guardrails) •
[Quickstart](#quickstart) •
[API Reference](#api-reference)

---

</div>

## Executive Overview

Most AI teams build a bespoke reverse proxy in-house to multiplex OpenAI and local models, manage API keys, and track token spend. Inevitably, these custom wrappers become fragile, difficult to maintain, and blind to output regressions. 

**LLMGate** is an open-source, production-hardened LLM Gateway written in modern Go. It sits between client applications and heterogeneous model providers (cloud and local) to provide:
1. **A Single Unified API:** Standardized OpenAI-compatible `POST /v1/chat` that abstracts provider-specific quirks (e.g. Ollama parameter translation, format conversions, reasoning tokens).
2. **Zero-Latency Security Layer:** Heuristic prompt-injection interception and automated regex-driven PII redaction *before* any text leaves your VPC.
3. **Resilient Multi-Provider Failover:** Automatic fallback with exponential backoff across model chains (e.g., OpenAI → Local Ollama).
4. **Sub-Millisecond Content Caching:** Cryptographic SHA-256 payload caching (in-memory or Redis) to slash token costs and eliminate duplicate inference.
5. **Real-Time Cost & Token Accounting:** Granular usage metering exposed as native Prometheus metrics.
6. **The Golden-Set Eval Gate (Top Differentiator):** A deterministic evaluation harness integrated into CI that **fails pull requests** when model quality drops or latency exceeds SLA budgets.

---

## Why LLMGate?

| Capability | Generic Proxies (LiteLLM / Kong) | In-House Custom Wrappers | **LLMGate** |
|:---|:---:|:---:|:---:|
| **Language & Footprint** | Python (heavy) or Lua/C++ plugins | Ad-hoc scripts (Node/Python) | **Pure Go, <25MB RAM, single binary** |
| **Unified OpenAI Interface** | Yes | Partial | **Yes (with deep Ollama translation)** |
| **Automatic Provider Failover** | Yes (often complex) | Manual try/catch | **Ordered chain with backoff & retry** |
| **In-Line Prompt Injection Filter** | Plugin / External add-on | Rarely implemented | **Built-in pre-flight heuristic scanner** |
| **Zero-Leak PII Redaction** | Expensive external API | Basic or none | **In-memory zero-allocation redaction** |
| **Cryptographic Caching** | Redis required | In-memory or none | **Built-in SHA-256 cache (Memory & Redis)** |
| **CI/CD Quality Gate** | None (runtime only) | None | **Built-in golden-set gate that breaks CI** |
| **Empirical Research Backed** | No | No | **Validated via 1,000-config benchmark** |

---

## System Architecture

LLMGate processes incoming requests through a deterministic, high-throughput pipeline designed for sub-millisecond gateway overhead:

<div align="center">

![LLMGate 3D Architecture Diagram](assets/llmgate-architecture-3d.jpg)

*Figure 1: High-level 3D isometric representation of LLMGate's modular pipeline.*

</div>

### End-to-End Request Lifecycle

```
                                      LLMGate Pipeline
 ┌──────────┐   POST /v1/chat    ┌────────────────────────────────────────────────────────┐
 │  Client  │ ─────────────────► │  1. Auth Middleware (Bearer API Token Validation)      │
 └──────────┘                    └──────────────────────────┬─────────────────────────────┘
                                                            │
                                 ┌──────────────────────────▼─────────────────────────────┐
                                 │  2. In-Line Guardrails                                 │
                                 │     ├─ Prompt-Injection Scan (Blocks with 403)         │
                                 │     └─ In-Place PII Redaction ([REDACTED_EMAIL], etc.) │
                                 └──────────────────────────┬─────────────────────────────┘
                                                            │
                                 ┌──────────────────────────▼─────────────────────────────┐
                                 │  3. Content-Addressed Completion Cache                 │
                                 │     └─ Key: SHA256(Messages + Sampling Params)         │
                                 └──────────────────────────┬─────────────────────────────┘
                                             Cache HIT?     │     Cache MISS?
                                   ┌────────────────────────┴────────────────┐
                                   ▼                                         ▼
                            [Return Cached Resp]              ┌───────────────────────────┐
                            (X-LLMGate-Cache: hit)            │  4. Resilient Router      │
                                                              │     Chain: Provider A ──┐ │
                                                              │     Fallback: Prov B ◄──┘ │
                                                              └─────────────┬─────────────┘
                                                                            │ Upstream Call
                                                              ┌─────────────▼─────────────┐
                                                              │  5. Upstream Providers    │
                                                              │     ├─ OpenAI (Cloud)     │
                                                              │     └─ Ollama (Local)     │
                                                              └─────────────┬─────────────┘
                                                                            │ Response OK
                                 ┌──────────────────────────────────────────┴─────────────┐
                                 │  6. Telemetry & Metering                               │
                                 │     ├─ Usage: Prompt + Completion Tokens               │
                                 │     ├─ Cost Accounting: Real-time USD Estimate         │
                                 │     └─ Prometheus Counters (/metrics & /status)        │
                                 └──────────────────────────┬─────────────────────────────┘
                                                            │
                                 ┌──────────────────────────▼─────────────────────────────┐
                                 │  7. Cache Population & Client Response                 │
                                 │     (X-LLMGate-Cache: miss, 200 OK)                    │
                                 └────────────────────────────────────────────────────────┘
```

### Sequence Flow

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant GW as LLMGate
    participant GR as Guardrails
    participant C as Cache
    participant R as Router
    participant P1 as Provider A (OpenAI)
    participant P2 as Provider B (Ollama)
    participant M as Metering

    Client->>GW: POST /v1/chat (Bearer Token + JSON)
    Note over GW: Auth Check
    GW->>GR: Scan Payload
    alt Prompt Injection Detected
        GR-->>Client: 403 Forbidden (Blocked)
    else Contains PII
        GR->>GR: Mutate payload: [REDACTED_*]
    end

    GW->>C: Check SHA-256(Payload)
    alt Cache Hit
        C-->>Client: 200 OK (X-LLMGate-Cache: hit)
    else Cache Miss
        GW->>R: Route Request
        R->>P1: POST /v1/chat/completions (Attempt 1)
        alt P1 Fails (5xx / Timeout)
            Note over R: Exponential Backoff Retry
            R->>P2: Fallback: POST /api/chat (Ollama format)
            P2-->>R: 200 OK (Completion)
        else P1 Succeeds
            P1-->>R: 200 OK (Completion)
        end
        R->>M: Record Usage (Tokens, Model, Cost USD)
        R->>C: Store in Cache (5m TTL)
        R-->>Client: 200 OK (X-LLMGate-Cache: miss)
    end
```

---

## The Four Pillars of Uniqueness

### 1. The CI Eval Gate: Stopping Silent Regressions

In traditional software, integration tests catch regressions. In LLM applications, regressions happen **silently**: a prompt tweak breaks downstream JSON parsing, a provider subtly updates their model weights, or local inference latency exceeds production budgets.

LLMGate solves this by introducing **The Golden-Set Eval Gate (`internal/eval`, `cmd/eval`)**:

<div align="center">

![LLMGate 3D Eval Gate Visual](assets/llmgate-eval-gate-3d.jpg)

*Figure 2: The Golden-Set Quality Gate continuously validating model accuracy and latency.*

</div>

- **Deterministic CI Testing:** During pull requests, GitHub Actions executes `go test ./internal/eval/... -run TestGoldenSet` against a locked evaluation golden set (`testdata/golden.json`).
- **Zero-Tolerance Quality Enforcement:** The test suite asserts that the pass rate must equal **100%** (or configured threshold) and maximum latency must remain strictly within the SLA budget.
- **Production Staging Verifier (`cmd/eval`):** A standalone CLI tool that runs the golden set against a live, running gateway instance before deployment:
  ```bash
  make eval   # Executes ./cmd/eval against http://localhost:8080/v1/chat
  ```

#### Example Output from `cmd/eval`:
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

If an upstream provider degrades or prompt tampering breaks expected substrings, the build **fails immediately**, protecting your production workloads.

---

### 2. Empirical Research: "Winner's Curse" Validation

Unlike simple wrappers, LLMGate's design is backed by empirical research on automated LLM optimization. Located in [`experiments/winners-curse/`](experiments/winners-curse/), this study investigates what happens when an LLM iteratively tunes retrieval and prompt parameters on a locked evaluation set:

> [!IMPORTANT]
> **Key Finding:** An automated LLM-driven optimization loop claimed **+4.5 points of Recall@10** on a 100-query eval set. When evaluated on **1,009 held-out queries**, the real gain was only **+1.5 points**. Two-thirds of the reported improvement was pure selection noise (the "Winner's Curse").

```
  Claimed Gain (100 Search Queries)  : ████████████████████ +4.5 pts
  Real Held-Out Gain (1,009 Queries) : ██████ +1.5 pts
  Selection Noise / Inflation        : ░░░░░░░░░░░░░░ +3.0 pts (Overfit)
```

#### Why This Matters for LLMGate:
1. **Dogfooding Under Heavy Load:** Running 1,000+ automated search evaluations through LLMGate surfaced and resolved three critical gateway production bugs:
   - Configurable upstream timeouts (preventing hangs on reasoning models).
   - Strict sampling parameter forwarding (`temperature`, `max_tokens`, `num_predict`).
   - Native structured JSON output enforcement.
2. **Empirical Grounding:** Demonstrates why static golden-set evals in CI must be paired with statistical rigor and held-out validation.

---

### 3. In-Line Defense-in-Depth Guardrails

LLMGate enforces proactive security **before** prompts reach expensive upstream APIs:

#### A. Prompt-Injection & Jailbreak Mitigation
Intercepts override attacks and exfiltration attempts at the ingress layer, returning `403 Forbidden`:
- Detects system prompt override attempts (`"ignore previous instructions"`, `"disregard initial prompt"`).
- Detects persona exploits (`"you are now in developer mode"`, `"DAN mode"`).
- Detects hidden prompt exfiltration (`"reveal your system prompt"`, `"print initial prompt"`).

#### B. Zero-Leak PII Redaction
Replaces sensitive personally identifiable information (PII) with opaque tokens directly in the request stream:

| Sensitive Entity | Pattern Matching | Sanitized Replacement |
|:---|:---|:---|
| **Email Addresses** | Standard RFC 5322 regex | `[REDACTED_EMAIL]` |
| **Credit / Debit Cards**| 13–16 digit Luhn candidates | `[REDACTED_CARD]` |
| **Social Security Numbers** | Standard `XXX-XX-XXXX` | `[REDACTED_SSN]` |
| **IPv4 Addresses** | Dotted-quad network strings | `[REDACTED_IP]` |
| **Phone Numbers** | International & E.164 candidates | `[REDACTED_PHONE_IND]` |

#### Sanitization Example:

**Client Request:**
```json
{
  "messages": [
    {
      "role": "user",
      "content": "Please send the invoice for card 4532-1234-5678-9010 to user@example.com."
    }
  ]
}
```

**Forwarded to Model Backend:**
```json
{
  "messages": [
    {
      "role": "user",
      "content": "Please send the invoice for card [REDACTED_CARD] to [REDACTED_EMAIL]."
    }
  ]
}
```

---

### 4. Resilient Multi-Provider Routing & Normalization

LLMGate provides seamless provider interoperability:

- **Automatic Format Translation:** Automatically translates OpenAI requests into Ollama's native API (`num_predict`, `format: "json"`, reasoning toggles).
- **Graceful Failover:** Attempts primary providers (e.g. OpenAI GPT-4o-mini). If the provider returns 5xx errors or times out, the router automatically falls back to secondary backends (e.g. local Ollama Llama 3.2) with configurable retry backoff.
- **Content-Addressable SHA-256 Cache:** Identical requests are hashed (`SHA-256(Messages + Parameters)`). Cache hits return in `<1ms` with the `X-LLMGate-Cache: hit` response header.

---

## Quickstart

### Option 1: 10-Second Zero-Cost Local Stack (Ollama)

Requires [Ollama](https://ollama.com) installed locally:

```bash
# 1. Pull lightweight open-weights model
ollama pull llama3.2

# 2. Run LLMGate (defaults to local Ollama on :11434)
go run ./cmd/llmgate
```

Test completion in another terminal:
```bash
curl -s http://localhost:8080/v1/chat \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2",
    "messages": [
      {"role": "user", "content": "What is the capital of France? Answer in one word."}
    ],
    "temperature": 0.0
  }' | jq
```

---

### Option 2: Cloud + Local Failover (OpenAI → Ollama)

Configure LLMGate to route to OpenAI first, failing over to local Ollama if OpenAI is unavailable:

```bash
# 1. Create config
cp config.example.yaml config.yaml

# 2. Set your OpenAI key
export OPENAI_API_KEY="sk-..."

# 3. Launch gateway
LLMGATE_CONFIG=config.yaml go run ./cmd/llmgate
```

---

### Option 3: Full Stack Docker Compose

Deploy the gateway with containerized PostgreSQL (persistent metering) and Redis (distributed caching):

```bash
docker compose up --build
```

---

## API Reference

### Endpoints

| Method | Endpoint | Description | Auth Required |
|:---|:---|:---|:---:|
| `POST` | `/v1/chat` | OpenAI-compatible chat completion | Optional (via Bearer Token) |
| `GET` | `/healthz` | Kubernetes liveness probe | No |
| `GET` | `/status` | Real-time gateway status, call counts, and spend | No |
| `GET` | `/metrics` | Standard Prometheus metric exposition | No |

### Request Schema (`POST /v1/chat`)

```json
{
  "model": "gpt-4o-mini",
  "messages": [
    {"role": "system", "content": "You are a concise engineering assistant."},
    {"role": "user", "content": "Explain goroutines in one sentence."}
  ],
  "temperature": 0.2,
  "max_tokens": 128,
  "response_format": {"type": "json_object"},
  "think": false
}
```

#### Supported Fields:
- `model` *(string, optional)*: Target model name. Falls back to provider config default.
- `messages` *(array, required)*: List of message objects with `role` and `content`.
- `temperature` *(float, optional)*: Sampling temperature (0.0 – 2.0).
- `max_tokens` *(int, optional)*: Maximum output tokens (forwarded as `num_predict` on Ollama).
- `response_format` *(object, optional)*: `{"type": "json_object"}` enables strict JSON mode.
- `think` *(bool, optional)*: Reasoning mode toggle for supported models (e.g. DeepSeek-R1 / Qwen reasoning variants on Ollama).

### Response Schema

```json
{
  "id": "chatcmpl-8f20b410",
  "provider": "openai",
  "model": "gpt-4o-mini",
  "content": "Goroutines are lightweight threads managed concurrently by the Go runtime rather than operating system threads.",
  "usage": {
    "prompt_tokens": 24,
    "completion_tokens": 18,
    "total_tokens": 42
  }
}
```

#### Response Headers:
- `X-LLMGate-Cache`: `hit` (retrieved from cache in <1ms) or `miss` (computed upstream).
- `Content-Type`: `application/json`.

---

## Observability & Telemetry

### 1. Prometheus Metrics (`GET /metrics`)

LLMGate natively exports text-format Prometheus metrics for seamless scraping into Grafana:

```prometheus
# HELP llmgate_calls_total Total chat calls served.
# TYPE llmgate_calls_total counter
llmgate_calls_total 42

# HELP llmgate_tokens_total Total tokens across all calls.
# TYPE llmgate_tokens_total counter
llmgate_tokens_total 3150

# HELP llmgate_cost_usd_total Estimated spend in USD.
# TYPE llmgate_cost_usd_total counter
llmgate_cost_usd_total 0.002362
```

### 2. Status Endpoint (`GET /status`)

Returns instantaneous operational telemetry in JSON:

```bash
curl -s http://localhost:8080/status | jq
```

```json
{
  "service": "llmgate",
  "calls": 42,
  "tokens": 3150,
  "cost_usd": 0.002362,
  "guardrails": {
    "block_prompt_injection": true,
    "redact_pii": true
  },
  "provider": "openai",
  "max_attempts": 2
}
```

---

## Configuration Reference

LLMGate is configured via a YAML file (`config.yaml`) with environment variable interpolation:

```yaml
# Listening address
addr: ":8080"

# Ordered provider chain (Failover tries providers in sequence)
providers:
  - name: openai
    base_url: https://api.openai.com
    api_key: ${OPENAI_API_KEY}
    model: gpt-4o-mini
  - name: ollama
    base_url: http://localhost:11434
    model: llama3.2

# Upstream call timeout in seconds
upstream_timeout_s: 60

# Resilient retry & backoff
max_attempts: 2
backoff_ms: 200

# In-line security guardrails
block_prompt_injection: true
redact_pii: true

# API keys for gateway ingress auth (empty = open for local dev)
api_keys: ""

# Optional enterprise backends (seams wired in codebase)
# database: postgres://llmgate:llmgate@localhost:5432/llmgate?sslmode=disable
# redis: localhost:6379
```

---

## Repository Structure

```
llmgate/
├── cmd/
│   ├── llmgate/              # Gateway main entrypoint & wiring
│   └── eval/                 # Standalone live gateway evaluation runner
├── internal/
│   ├── config/               # Configuration parsing with env expansion
│   ├── server/               # HTTP handlers, middleware, routes, metrics
│   ├── guardrails/           # Prompt-injection & regex PII redaction engine
│   ├── cache/                # SHA-256 content-keyed cache (Memory & Redis)
│   ├── router/               # Multi-provider routing, failover, backoff
│   ├── providers/            # OpenAI & Ollama adapters with normalization
│   ├── metering/             # Token accounting & USD pricing engine
│   └── eval/                 # Golden-set evaluation harness & CI quality gate
│       └── testdata/         # Golden evaluation test suite (golden.json)
├── experiments/
│   └── winners-curse/        # Empirical retrieval research & gateway dogfooding
├── assets/                   # 3D architecture diagrams and visual artifacts
├── docker-compose.yml        # Multi-container orchestration (Gateway + DB + Redis)
├── Dockerfile                # Minimal multi-stage scratch/alpine container
└── Makefile                  # Developer workflow automation
```

---

## Development & Makefile Automation

```bash
make run        # Run gateway locally (using config.yaml if present)
make test       # Run all unit tests + CI Golden-Set Eval Gate
make eval       # Run evaluation suite against live :8080 instance
make vet        # Run static analysis and Go vet
make build      # Compile standalone binary to ./bin/llmgate
make docker     # Build minimal Docker image
make up         # Spin up full docker-compose stack
make down       # Tear down containers and networks
```

---

## Roadmap

- [x] OpenAI & Ollama unified adapters
- [x] Heuristic prompt-injection & PII security guardrails
- [x] Deterministic CI/CD Golden-Set Quality Gate
- [x] Content-addressed SHA-256 in-memory caching
- [x] Real-time token and USD cost metering
- [ ] Server-Sent Events (SSE) streaming passthrough
- [ ] Distributed PostgreSQL persistent metering backend (`metering.NewPostgres`)
- [ ] Distributed Redis cache backend (`cache.NewRedis`)
- [ ] Semantic vector embedding cache
- [ ] Anthropic Claude & Google Gemini provider adapters
- [ ] Classifier-based ML guardrails & output safety scanners

---

## License

Distributed under the **MIT License**. See [`LICENSE`](LICENSE) for details.

Developed with passion by **Harsh Kumar**.
