# Winner's curse in automated retrieval search

When an automated search picks the best of N candidates on a small, locked
eval set, part of that "best" score is luck: the winner is partly the
candidate that happened to suit those particular queries. This experiment
measures how big that effect is on a real retrieval benchmark, and how it
grows with the number of candidates N and shrinks with the eval size S.

It is a small, honest version of a question every autonomous research loop
has to answer: **how much of the reported improvement survives on data the
search never saw?**

## TL;DR

- **An LLM-driven search claimed +4.5 points of Recall@10. On 1,009 held-out
  queries the real gain was +1.5** (95% CI −0.04 to +2.9). Two-thirds of the
  reported improvement was selection noise. The setup: qwen3:4b proposing
  BM25 configs through LLMGate, 60 candidates, a 100-query locked eval.
- **The config that was actually best on held-out queries lost the search.**
  It scored 0.830 on the search split against the winner's 0.850, so a
  100-query eval could not tell them apart and kept the wrong one.
- **Across four splits, the LLM search claimed +4.0 points on average and
  delivered +0.6.** No LLM run's real gain had a 95% CI that excludes zero.
  In the worst run it claimed +8.0 and delivered +0.1.
- **Across 1,000 random configs and 200 random splits**, optimism grows with
  every extra candidate and shrinks as the eval grows. With a 25-query eval,
  the best of 999 candidates claimed +8.3 points and delivered −0.5. **Half the time, the "winner" was no
  better than plain BM25.** With 400 queries, the false-win rate drops to 2%
  and the real gain rises to +1.6. A bigger eval doesn't just shrink the
  inflation, it picks better configs.
- **Running a real search through LLMGate surfaced three gateway bugs**: a
  hard-coded 60s timeout, silently dropped `temperature`/`max_tokens` for
  Ollama, and no JSON mode. Fixing them took a proposal from over 2 minutes
  (and failed calls) to about 7 seconds, with 0 gateway errors across 236
  calls.

## Setup

| | |
|---|---|
| Data | [BEIR SciFact](https://github.com/beir-cellar/beir): 5,183 abstracts, 1,109 queries with relevance labels (train + test qrels) |
| System | BM25 over title + abstract. Tunable: `k1`, `b`, title weight, stopwords, stemming, RM3-style pseudo-relevance feedback (docs, terms, mix weight) |
| Metric | Recall@10 (nDCG@10 is also recorded in `results.json`) |
| Split | S queries form the **search split**. The rest are **held out** |
| Baseline | Plain BM25 (`k1=0.9, b=0.4`), always candidate 0 |
| Proposers | `llm`: a model proposes the next config through **LLMGate** given the search-split history. `random`: uniform samples from the space |

**Held-out isolation.** The proposer and the selection step only ever see
search-split scores (`run_search` in `experiment.py`). Held-out scores are read
once, after the search ends. Per-query scores are computed for every query in
one pass for speed, and the code only slices the held-out part for reporting.

Terms used below:

- **Claimed gain**: the pick's search-split score minus baseline's search-split score. This is what the search would report.
- **Real gain**: the same difference on held-out queries.
- **Gain inflation**: claimed minus real.
- **Optimism**: the pick's own search score minus its held-out score.
- **False win**: the search reports a gain, but on held-out queries the pick is no better than baseline.

## Results

### 1. One LLM-driven search

qwen3:4b, reasoning off, proposes each config through LLMGate. It sees the 8
best configs so far and the last 5, scored on the search split only.
100 search queries, 1,009 held out, 60 candidates, split seed 7.

| | search split | held out |
|---|---|---|
| Baseline BM25 | 0.8050 | 0.7881 |
| Picked (best on search) | 0.8500 | 0.8028 |
| Oracle (best on held-out) | 0.8300 | 0.8167 |

![LLM search: search score vs held-out score of the pick](results/llm/curve.svg)

- Claimed gain **+4.5 pts**, real gain **+1.5 pts** (95% CI −0.04 to +2.9),
  inflation +3.0.
- The first new "best" the search found (candidate 3) was +1.0 on the
  search split and −0.7 on held-out, so it was *worse* than baseline. The
  switch from candidate 4 to candidate 6 added a point on the search split
  while the held-out score went down.
- The search and held-out rankings of the 60 candidates correlate at 0.69,
  against 0.84 for random search on the same split. The LLM's candidates
  cluster close together, so a 100-query eval has a harder time telling them
  apart. This is one run, so treat it as an observation, not a finding.

### 2. The same search on four splits

Same budget (60 candidates, 100 search queries) on four random splits. The
LLM and random proposers are paired on each split. Regenerate the table with
`make compare`.

| proposer | split seed | claimed gain | real gain (95% CI) | inflation |
|---|---|---|---|---|
| llm | 7 | +4.5 | +1.5 (-0.0 to +2.9) | +3.0 |
| llm | 11 | +8.0 | +0.1 (-1.5 to +1.7) | +7.9 |
| llm | 13 | +2.2 | +0.6 (-1.0 to +2.0) | +1.6 |
| llm | 17 | +1.5 | +0.3 (-1.6 to +2.1) | +1.2 |
| random | 7 | +3.0 | +0.5 (-0.8 to +1.8) | +2.5 |
| random | 11 | +5.0 | +1.3 (+0.0 to +2.7) | +3.7 |
| random | 13 | +0.8 | -0.8 (-2.6 to +0.8) | +1.7 |
| random | 17 | +2.0 | -1.2 (-2.6 to +0.2) | +3.2 |
| **llm, mean** | 4 splits | +4.0 | +0.6 | +3.4 |
| **random, mean** | 4 splits | +2.7 | -0.0 | +2.7 |

- On average the LLM search reported +4.0 points and delivered +0.6. About
  85% of what it reported did not survive held-out queries.
- **No LLM run has a real gain whose 95% CI excludes zero.** Only one of the
  eight runs does (random, seed 11), with a lower bound of +0.05.
- Seed 11 is the cautionary case: the LLM search claimed +8.0 and delivered
  +0.1.
- The LLM proposer claimed more than random and delivered a little more.
  With four splits and overlapping CIs, that difference is within noise.
- Across all four LLM runs: 236 gateway calls, **0 gateway errors**, 12
  unparseable replies (5%), about 1,200 tokens per proposal.

### 3. How the curse scales with N and S

A pool of 1,000 random configs, each scored once per query. For each
search-split size S, 200 random splits. Candidates are taken in random order
and the best-on-search pick is recorded after N of them. Full output:
[`results/sweep/summary.md`](results/sweep/summary.md).

**Optimism of the pick** (its search score minus its held-out score, Recall@10 points):

| search queries (S) | N=1 | N=5 | N=10 | N=25 | N=50 | N=100 | N=250 | N=500 | N=999 |
|---|---|---|---|---|---|---|---|---|---|
| 25 | -0.3 | +3.4 | +4.7 | +5.8 | +6.4 | +6.9 | +7.7 | +8.1 | +8.5 |
| 50 | -0.2 | +2.2 | +3.1 | +3.9 | +4.6 | +5.0 | +5.5 | +5.8 | +6.2 |
| 100 | +0.1 | +1.5 | +2.1 | +2.6 | +3.0 | +3.3 | +3.7 | +4.0 | +4.2 |
| 200 | +0.1 | +0.9 | +1.2 | +1.7 | +1.9 | +2.0 | +2.2 | +2.5 | +2.7 |
| 400 | -0.0 | +0.5 | +0.8 | +1.0 | +1.1 | +1.3 | +1.5 | +1.6 | +1.7 |

**False-win rate** (the search reports a gain, but the pick is no better than baseline on held-out queries):

| search queries (S) | N=1 | N=5 | N=10 | N=25 | N=50 | N=100 | N=250 | N=500 | N=999 |
|---|---|---|---|---|---|---|---|---|---|
| 25 | 0% | 36% | 43% | 51% | 52% | 54% | 54% | 53% | 52% |
| 50 | 0% | 34% | 42% | 46% | 49% | 46% | 44% | 42% | 39% |
| 100 | 0% | 33% | 39% | 34% | 32% | 29% | 23% | 20% | 20% |
| 200 | 0% | 25% | 28% | 28% | 21% | 15% | 13% | 10% | 6% |
| 400 | 0% | 23% | 30% | 23% | 16% | 10% | 8% | 5% | 2% |

**Claimed vs real gain over baseline at N=999** (points):

| search queries (S) | claimed (search) | real (held out) | real / claimed |
|---|---|---|---|
| 25 | +8.3 | −0.5 | — |
| 50 | +6.7 | +0.3 | 4% |
| 100 | +5.0 | +0.9 | 18% |
| 200 | +3.9 | +1.3 | 34% |
| 400 | +3.3 | +1.6 | 48% |

![Optimism vs N for each eval size](results/sweep/optimism.svg)

What the tables say:

- N=1 is the sanity check. The pick is always the baseline, and its optimism
  averages out to about zero over random splits.
- Optimism grows with every extra candidate and never levels off inside this
  range. **More search makes the reported number less trustworthy unless the
  eval grows with it.**
- Quadrupling the eval (100 → 400 queries) cuts optimism by more than half at
  every N.
- At small N the false-win rate is high because most random configs are worse
  than baseline. The best of a few barely beats baseline on the search split,
  and that edge is mostly noise.

### 4. What broke when I ran a real search through LLMGate

The experiment sends every proposal through LLMGate, so it doubled as a load
test of the gateway on a real workload. Three things broke:

1. **Hard-coded 60s upstream timeout.** With reasoning on, one proposal from
   qwen3:4b took 2m10s and 3,252 tokens. The first attempt hit the 60s
   timeout, the router retried, the retry timed out too, and the proposal
   failed. *Fix:* `upstream_timeout_s` in config, or `LLMGATE_UPSTREAM_TIMEOUT_S`.
2. **The Ollama provider silently dropped `temperature` and `max_tokens`.**
   Requests went out with Ollama's defaults no matter what the client asked
   for. *Fix:* map them to Ollama's `options` (`temperature`, `num_predict`).
   Also stopped sending `max_tokens: 0` to OpenAI, which rejects it.
3. **No JSON mode or reasoning control.** With reasoning off, qwen3:4b still
   reasoned in plain text before answering: a one-line JSON reply took about
   4 minutes. *Fix:* OpenAI-style `response_format: {"type": "json_object"}`,
   mapped to Ollama's `format: "json"`, plus a `think` passthrough. The same
   call now takes 0.97s and 23 tokens.

After the fixes, the four LLM runs made 236 gateway calls with **0 gateway
errors**, a 6–7s mean latency and about 1,200 tokens per proposal. Twelve
replies (5%) didn't parse into a config and fell back to a mutation of the
current best. Each run's `results.json` counts them.

## What I'd do about it

- **Never report the max.** Report the pick's score on a held-out split the
  search never touched, with a paired bootstrap CI against baseline.
- **Size the locked eval for the gain you want to claim.** The sweep table
  shows how much optimism to expect for a given S and N. If the gain you are
  claiming is smaller than that, the eval is too small to support it.
- **Re-test the finalists.** Re-score the top-k candidates on fresh queries
  before picking (successive halving). This costs a little compute and removes
  most of the selection bias.
- **Pick by a lower confidence bound, not the mean**, so a noisy lucky
  candidate loses to a slightly worse but stable one.
- **Rotate the held-out set** when the search runs for a long time. Each look
  at a held-out set leaks a little information into the next round of
  choices (see the reusable holdout below).
- **Track the search vs held-out gap as a CI metric**, the same way
  LLMGate's golden-set eval gate fails a build on regression.

Background: Smith & Winkler, *The Optimizer's Curse* (Management Science,
2006); Cawley & Talbot, *On Over-fitting in Model Selection and Subsequent
Selection Bias in Performance Evaluation* (JMLR, 2010); Dwork et al., *The
reusable holdout* (Science, 2015).

## Reproduce

Needs Python 3 with numpy, plus Go and [Ollama](https://ollama.com) for the
LLM run.

```bash
make data      # download SciFact (2.8 MB) into data/scifact
make sweep     # random-search pool replayed over many splits (no LLM needed)
make random    # one 60-candidate random search on the default split
make compare   # claimed vs real gain across every search run

# LLM proposer through LLMGate (from the repo root, in another terminal)
ollama pull qwen3:4b
go build -o bin/llmgate ./cmd/llmgate
LLMGATE_ADDR=:8090 LLMGATE_UPSTREAM_TIMEOUT_S=300 ./bin/llmgate

make llm       # 60-candidate LLM search, reasoning off
make seeds     # paired random + LLM searches on three more splits
```

LLM runs are sampled, so a rerun gives different candidates. Random runs and
the sweep are deterministic for a given seed.

Each command writes `summary.md`, a JSON file with every candidate, and an SVG
chart under `results/`.

## Limits

- One dataset and one system family. The BM25 variants here are highly
  correlated with each other, which keeps the curse small. More diverse
  candidates, like generated code, are untested here.
- The held-out queries come from the same distribution as the search split.
  This measures selection noise, not distribution shift.
- Four LLM runs is a small sample. The sweep, which uses random search, is
  where the scaling claims come from.
- The LLM proposer is a small local model (qwen3:4b, reasoning off). A
  stronger proposer may find better configs, and it may also exploit noise
  harder. Both are untested here.
