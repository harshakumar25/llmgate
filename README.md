# LLMGate

A small, readable, self-hostable **LLM gateway in Go**. One OpenAI-compatible
API in front of any model backend, with provider failover, cost + token
metering, response caching, and a security layer — prompt-injection guardrails
and PII redaction — plus a **golden-set eval that gates CI**.

> Most AI teams build a worse version of this in-house. LLMGate is a clean
> reference you can self-host, read in an afternoon, and extend.

[![ci](https://github.com/harshakumar25/llmgate/actions/workflows/eval.yml/badge.svg)](https://github.com/harshakumar25/llmgate/actions/workflows/eval.yml)
![go](https://img.shields.io/badge/go-1.26-00ADD8)
![license](https://img.shields.io/badge/license-MIT-green)

<!-- TODO: replace with a 60-second demo GIF: a request routed, a provider
failing over, the /metrics endpoint ticking up. `asciinema` or a screen record
→ GIF works well. This is the single highest-impact thing in the README. -->

---

## Why it exists

| Capability | What LLMGate does |
|---|---|
| **One API** | OpenAI-compatible `POST /v1/chat` in front of every provider |
| **Failover** | Tries providers in order, retries with backoff, fails over on error |
| **Cost control** | Per-key token + USD metering, exposed at `/metrics` and `/status` |
| **Caching** | Identical requests skip the provider call (in-memory → Redis) |
| **Security** | Inbound prompt-injection block + PII redaction before any model sees the text |
| **Quality gate** | A golden-set eval runs in CI and **fails the build** on regression |

## Architecture

```
                 ┌─────────────────────────────────────────────┐
  client ──────► │  server   auth → guardrails → cache → router │ ──► provider A (OpenAI)
  POST /v1/chat  │                              │               │ ──► provider B (Ollama)
                 │                           metering           │      (failover + retry)
                 └─────────────────────────────┬───────────────┘
                                   /metrics  /status  /healthz
```

- **server** — HTTP surface, API-key auth, JSON errors (`internal/server`)
- **guardrails** — prompt-injection detection + PII redaction (`internal/guardrails`)
- **cache** — content-keyed TTL cache, in-memory or Redis (`internal/cache`)
- **router** — ordered failover with retry/backoff (`internal/router`)
- **providers** — OpenAI + Ollama backends behind one interface (`internal/providers`)
- **metering** — token/cost accounting, in-memory or Postgres (`internal/metering`)
- **eval** — golden-set harness + CI gate (`internal/eval`, `cmd/eval`)

## Quickstart

**Local, zero cost** (needs [Ollama](https://ollama.com) running with a model):

```bash
ollama pull llama3.2
go run ./cmd/llmgate            # listens on :8080 against local Ollama
```

```bash
curl -s localhost:8080/v1/chat -d '{
  "messages": [{"role":"user","content":"What is the capital of France?"}]
}' | jq
```

**With OpenAI + failover:**

```bash
cp config.example.yaml config.yaml      # providers: openai then ollama
export OPENAI_API_KEY=sk-...
LLMGATE_CONFIG=config.yaml go run ./cmd/llmgate
```

**Full stack (gateway + Postgres + Redis):**

```bash
docker compose up --build
```

## API

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/chat` | Chat completion (OpenAI-compatible body) |
| `GET` | `/healthz` | Liveness |
| `GET` | `/status` | Human-readable JSON status + totals |
| `GET` | `/metrics` | Prometheus text exposition |

Request body:

```json
{ "model": "gpt-4o-mini", "messages": [{"role":"user","content":"..."}], "temperature": 0.2 }
```

Optional fields, forwarded in each provider's own format:

| Field | Effect |
|---|---|
| `max_tokens` | Output cap (`num_predict` on Ollama) |
| `response_format` | `{"type": "json_object"}` for JSON mode (`format: "json"` on Ollama) |
| `think` | `true` / `false` reasoning toggle for models that support it (Ollama) |

Set `api_keys` in config to require `Authorization: Bearer <key>`. Slow local
models may need a longer `upstream_timeout_s` (default 60, or
`LLMGATE_UPSTREAM_TIMEOUT_S`).

## The eval gate

`internal/eval` runs a JSON golden set (`testdata/golden.json`) through the
gateway and scores answers against expected substrings. CI runs it on every
push and **fails when the pass rate or latency budget regresses** — so a change
that makes answers worse can't merge silently.

```bash
make test                       # includes the deterministic CI eval
make eval                       # run the golden set against a live gateway
```

Add cases by appending to `testdata/golden.json`.

## Security

LLMGate filters requests *before* they reach a model:

- **Prompt-injection block** — heuristic patterns reject obvious override /
  jailbreak / prompt-exfiltration attempts (`403`).
- **PII redaction** — emails, cards, SSNs, phone numbers, IPs are replaced with
  `[REDACTED_*]` placeholders.

These are a foundation, not a finished product. Roadmap: a classifier-based
injection detector, per-tenant policy, output-side scanning, and audit logging.

## Experiments

- [Winner's curse in automated retrieval search](experiments/winners-curse/):
  an LLM-driven search through LLMGate claimed +4.5 points of Recall@10 on a
  100-query eval. On 1,009 held-out queries the real gain was +1.5. Includes a
  1,000-config sweep of how that optimism scales with the number of candidates
  and the eval size, and the three gateway bugs the run exposed.

## Roadmap

- [ ] Streaming (SSE) passthrough
- [ ] Postgres metering (`metering.NewPostgres` — seam is in place)
- [ ] Redis cache (`cache.NewRedis` — seam is in place)
- [ ] Semantic (embedding) cache
- [ ] Anthropic + Gemini providers
- [ ] Prometheus client_golang collectors
- [ ] Classifier-based guardrails

## Project layout

```
cmd/llmgate      gateway entrypoint
cmd/eval         standalone eval runner (live gateway)
internal/
  config         config loading (file + env)
  server         HTTP handlers, auth, metrics
  providers      OpenAI, Ollama, provider interface
  router         failover + retry
  guardrails     prompt-injection + PII
  metering       token/cost accounting
  cache          completion cache
  eval           golden-set harness + CI gate
```

## Development

```bash
make run        # run the gateway
make test       # unit tests + eval gate
make vet        # static checks
make up         # docker compose stack
```

## License

MIT © Harsh Kumar
