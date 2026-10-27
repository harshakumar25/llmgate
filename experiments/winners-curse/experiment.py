#!/usr/bin/env python3
"""Winner's curse in automated retrieval search.

Question: when an automated search picks the best of N retrieval configs on a
small "locked" eval set, how much of that best score survives on queries the
search never saw?

Setup
  data      BEIR SciFact: 5,183 abstracts, ~1,100 queries with qrels (train+test)
  system    BM25 over title + abstract with tunable k1, b, title weight,
            stopwords, stemming and RM3-style pseudo-relevance feedback
  metric    Recall@10 (nDCG@10 also recorded)
  split     S queries form the search split; the rest are held out
  proposer  "llm" asks a model through LLMGate for the next config given the
            search-split history; "random" samples the space uniformly

Commands
  search    one search run (LLM or random) on one split, with a full report
  sweep     random-search pool replayed over many splits, search-split sizes
            and candidate budgets: how optimism scales with N and S

Only numpy is required. Held-out scores never reach the proposer or the
selection step: see run_search, which only ever reads the search slice.
"""

import argparse
import heapq
import json
import math
import re
import time
import urllib.request
from collections import Counter, defaultdict
from pathlib import Path

import numpy as np

# --------------------------------------------------------------------------- #
# Config space
# --------------------------------------------------------------------------- #

SPACE = {
    "k1": ("float", 0.2, 3.0),
    "b": ("float", 0.0, 1.0),
    "title_weight": ("float", 0.0, 3.0),
    "stopwords": ("bool",),
    "stem": ("bool",),
    "prf_docs": ("int", 0, 10),
    "prf_terms": ("int", 0, 40),
    "prf_weight": ("float", 0.0, 0.9),
}

# Plain BM25 with common defaults: candidate 0 in every search.
BASELINE = {
    "k1": 0.9, "b": 0.4, "title_weight": 1.0, "stopwords": True, "stem": False,
    "prf_docs": 0, "prf_terms": 0, "prf_weight": 0.0,
}


def normalize(cfg):
    """Clamp to the space, round floats, and canonicalize "no PRF"."""
    out = {}
    for k, spec in SPACE.items():
        v = cfg.get(k, BASELINE[k])
        if spec[0] == "bool":
            out[k] = v if isinstance(v, bool) else str(v).lower() in ("true", "1", "yes")
        elif spec[0] == "int":
            out[k] = int(min(max(round(float(v)), spec[1]), spec[2]))
        else:
            out[k] = round(min(max(float(v), spec[1]), spec[2]), 2)
    if out["prf_docs"] == 0 or out["prf_terms"] == 0 or out["prf_weight"] == 0:
        out["prf_docs"], out["prf_terms"], out["prf_weight"] = 0, 0, 0.0
    return out


def key_of(cfg):
    return json.dumps(cfg, sort_keys=True)


def sample(rng):
    cfg = {}
    for k, spec in SPACE.items():
        if spec[0] == "bool":
            cfg[k] = bool(rng.integers(2))
        elif spec[0] == "int":
            cfg[k] = int(rng.integers(spec[1], spec[2] + 1))
        else:
            cfg[k] = float(rng.uniform(spec[1], spec[2]))
    return normalize(cfg)


def mutate(cfg, rng):
    out = dict(cfg)
    for k, spec in SPACE.items():
        if spec[0] == "bool":
            if rng.random() < 0.2:
                out[k] = not out[k]
        elif rng.random() < 0.5:
            out[k] = out[k] + rng.normal(0, 0.15 * (spec[2] - spec[1]))
    return normalize(out)


# --------------------------------------------------------------------------- #
# Data
# --------------------------------------------------------------------------- #

def load_scifact(root):
    root = Path(root)
    doc_ids, docs = [], []
    with open(root / "corpus.jsonl") as f:
        for line in f:
            d = json.loads(line)
            doc_ids.append(d["_id"])
            docs.append((d.get("title", ""), d.get("text", "")))
    doc_index = {d: i for i, d in enumerate(doc_ids)}

    texts = {}
    with open(root / "queries.jsonl") as f:
        for line in f:
            q = json.loads(line)
            texts[q["_id"]] = q["text"]

    rels = defaultdict(set)
    for name in ("train.tsv", "test.tsv"):
        with open(root / "qrels" / name) as f:
            next(f)  # header
            for line in f:
                qid, did, score = line.rstrip("\n").split("\t")
                if int(score) > 0 and did in doc_index:
                    rels[qid].add(doc_index[did])

    queries = [(qid, texts[qid], rels[qid]) for qid in sorted(rels, key=int) if qid in texts]
    return docs, queries


# --------------------------------------------------------------------------- #
# Retrieval
# --------------------------------------------------------------------------- #

STOPWORDS = set("""
a about above after again against all am an and any are as at be because been
before being below between both but by can could did do does doing down during
each few for from further had has have having he her here hers him his how i if
in into is it its itself just me more most my no nor not now of off on once only
or other our out over own same she should so some such than that the their them
then there these they this those through to too under until up very was we were
what when where which while who whom why will with would you your
""".split())

SUFFIXES = [
    ("ational", "ate"), ("ization", "ize"), ("fulness", "ful"), ("iveness", "ive"),
    ("ations", "ate"), ("ation", "ate"), ("ments", ""), ("ment", ""), ("ings", ""),
    ("ing", ""), ("edly", ""), ("ies", "y"), ("ied", "y"), ("ed", ""), ("es", ""),
    ("ly", ""), ("s", ""),
]

TOKEN = re.compile(r"[a-z0-9]+")


def light_stem(w):
    if len(w) <= 4 or w.isdigit() or w.endswith("ss"):
        return w
    for suf, rep in SUFFIXES:
        if w.endswith(suf) and len(w) - len(suf) >= 3:
            return w[: -len(suf)] + rep
    return w


def tokenizer(stopwords, stem):
    def tok(text):
        words = TOKEN.findall(text.lower())
        if stopwords:
            words = [w for w in words if w not in STOPWORDS]
        if stem:
            words = [light_stem(w) for w in words]
        return words
    return tok


class Index:
    """Two-field (title, body) inverted index plus a forward index for PRF."""

    def __init__(self, docs, stopwords, stem):
        self.tok = tokenizer(stopwords, stem)
        self.N = len(docs)
        vocab = {}
        body, title = [], []  # per term: (doc ids, tfs)
        df = []
        self.fwd = []
        body_len = np.zeros(self.N)
        title_len = np.zeros(self.N)

        def tid_of(t):
            if t not in vocab:
                vocab[t] = len(vocab)
                body.append(([], []))
                title.append(([], []))
                df.append(0)
            return vocab[t]

        for d, (ttl, txt) in enumerate(docs):
            tc, bc = Counter(self.tok(ttl)), Counter(self.tok(txt))
            title_len[d], body_len[d] = sum(tc.values()), sum(bc.values())
            for field, counts in ((body, bc), (title, tc)):
                for t, c in counts.items():
                    ids, tfs = field[tid_of(t)]
                    ids.append(d)
                    tfs.append(c)
            both = tc + bc
            tids = [tid_of(t) for t in both]
            for i in tids:
                df[i] += 1
            self.fwd.append((np.array(tids), np.array(list(both.values()), dtype=float)))

        self.vocab = vocab
        self.body = [(np.array(i), np.array(t, dtype=float)) for i, t in body]
        self.title = [(np.array(i), np.array(t, dtype=float)) for i, t in title]
        df = np.array(df, dtype=float)
        self.idf = np.log(1 + (self.N - df + 0.5) / (df + 0.5))
        self.body_len, self.title_len = body_len, title_len
        self.avg_body, self.avg_title = body_len.mean(), max(title_len.mean(), 1e-9)

    def query_weights(self, text):
        counts = Counter(self.tok(text))
        return {self.vocab[t]: float(c) for t, c in counts.items() if t in self.vocab}

    def norms(self, k1, b):
        nb = k1 * (1 - b + b * self.body_len / self.avg_body)
        nt = k1 * (1 - b + b * self.title_len / self.avg_title)
        return nb, nt

    def score(self, qw, k1, title_weight, nb, nt):
        s = np.zeros(self.N)
        for tid, w in qw.items():
            coef = w * self.idf[tid]
            ids, tf = self.body[tid]
            if len(ids):
                s[ids] += coef * tf * (k1 + 1) / (tf + nb[ids])
            ids, tf = self.title[tid]
            if title_weight > 0 and len(ids):
                s[ids] += title_weight * coef * tf * (k1 + 1) / (tf + nt[ids])
        return s

    def expand(self, qw, scores, n_docs, n_terms, lam):
        """RM3-style: mix the query with top terms from the top-ranked docs."""
        top = np.argpartition(-scores, n_docs)[:n_docs]
        acc = defaultdict(float)
        for d in top:
            tids, tfs = self.fwd[d]
            if not len(tids):
                continue
            vals = tfs / tfs.sum() * self.idf[tids]
            for t, v in zip(tids.tolist(), vals.tolist()):
                acc[t] += v
        best = heapq.nlargest(n_terms, acc.items(), key=lambda kv: kv[1])
        etot = sum(v for _, v in best) or 1.0
        qtot = sum(qw.values())
        new = {t: (1 - lam) * w / qtot for t, w in qw.items()}
        for t, v in best:
            new[t] = new.get(t, 0.0) + lam * v / etot
        return new


class Evaluator:
    """Per-query Recall@10 and nDCG@10 for a config, cached by config.

    Scores are computed for every query at once for speed; callers choose which
    slice they are allowed to read.
    """

    def __init__(self, docs, queries, k=10):
        self.docs, self.queries, self.k = docs, queries, k
        self.indexes = {}
        self.cache = {}

    def index_for(self, cfg):
        key = (cfg["stopwords"], cfg["stem"])
        if key not in self.indexes:
            self.indexes[key] = Index(self.docs, *key)
        return self.indexes[key]

    def per_query(self, cfg):
        ck = key_of(cfg)
        if ck in self.cache:
            return self.cache[ck]
        idx = self.index_for(cfg)
        nb, nt = idx.norms(cfg["k1"], cfg["b"])
        k = self.k
        rec = np.zeros(len(self.queries))
        ndcg = np.zeros(len(self.queries))
        discounts = 1 / np.log2(np.arange(k) + 2)
        for i, (_, text, rel) in enumerate(self.queries):
            qw = idx.query_weights(text)
            if not qw:
                continue
            s = idx.score(qw, cfg["k1"], cfg["title_weight"], nb, nt)
            if cfg["prf_docs"]:
                qw = idx.expand(qw, s, cfg["prf_docs"], cfg["prf_terms"], cfg["prf_weight"])
                s = idx.score(qw, cfg["k1"], cfg["title_weight"], nb, nt)
            top = np.argpartition(-s, k)[:k]
            top = top[np.argsort(-s[top])]
            hits = np.array([d in rel for d in top.tolist()], dtype=float)
            rec[i] = hits.sum() / len(rel)
            ndcg[i] = (hits * discounts).sum() / discounts[: min(len(rel), k)].sum()
        self.cache[ck] = (rec, ndcg)
        return rec, ndcg


# --------------------------------------------------------------------------- #
# Proposers
# --------------------------------------------------------------------------- #

class RandomProposer:
    name = "random"

    def __init__(self, rng):
        self.rng = rng

    def propose(self, history):
        return sample(self.rng)


class LLMProposer:
    """Asks a model, through LLMGate, for the next config.

    The prompt carries only search-split scores. Unparseable replies fall back
    to a random mutation of the current best and are counted.
    """

    name = "llm"

    SYSTEM = (
        "You are an automated research engineer tuning a BM25 retrieval system "
        "for scientific claim retrieval (SciFact). Your goal is to maximize "
        "Recall@10. Reply with ONE JSON object and nothing else."
    )

    def __init__(self, gateway, model, rng, think=False, timeout=300):
        self.url = gateway.rstrip("/")
        self.model, self.rng, self.think, self.timeout = model, rng, think, timeout
        self.calls = self.fallbacks = self.errors = 0
        self.latencies = []

    def prompt(self, history):
        space = "\n".join(
            f'- "{k}": {spec[0]}' + (f" in [{spec[1]}, {spec[2]}]" if spec[0] != "bool" else "")
            for k, spec in SPACE.items()
        )
        ranked = sorted(history, key=lambda h: -h["search_recall10"])[:8]
        fmt = lambda h: f'{json.dumps(h["config"], sort_keys=True)} -> Recall@10 {h["search_recall10"]:.4f}'
        return (
            f"Parameters:\n{space}\n"
            "prf_* enables pseudo-relevance feedback: expand the query with prf_terms terms "
            "from the top prf_docs documents, mixed in with weight prf_weight.\n\n"
            f"Best configs so far:\n" + "\n".join(map(fmt, ranked)) + "\n\n"
            f"Most recent configs:\n" + "\n".join(map(fmt, history[-5:])) + "\n\n"
            "Propose one new configuration that is different from all of the above. "
            "Return only the JSON object with exactly these keys."
        )

    def chat(self, content):
        body = json.dumps({
            "model": self.model,
            "messages": [{"role": "system", "content": self.SYSTEM},
                         {"role": "user", "content": content}],
            "temperature": 0.7,
            "max_tokens": 4096 if self.think else 300,
            "response_format": {"type": "json_object"},
            "think": self.think,
        }).encode()
        req = urllib.request.Request(self.url + "/v1/chat", data=body,
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=self.timeout) as r:
            return json.loads(r.read())["content"]

    def propose(self, history):
        self.calls += 1
        t0 = time.time()
        try:
            reply = self.chat(self.prompt(history))
        except Exception as e:  # noqa: BLE001 - any upstream failure falls back
            self.errors += 1
            print(f"  gateway error: {e}")
            reply = ""
        self.latencies.append(time.time() - t0)
        reply = re.sub(r"<think>.*?</think>", "", reply, flags=re.S)
        m = re.search(r"\{.*?\}", reply, flags=re.S)
        if m:
            try:
                return normalize(json.loads(m.group(0)))
            except (ValueError, TypeError):
                pass
        self.fallbacks += 1
        best = max(history, key=lambda h: h["search_recall10"])["config"]
        return mutate(best, self.rng)


def gateway_status(url):
    with urllib.request.urlopen(url.rstrip("/") + "/status", timeout=10) as r:
        return json.loads(r.read())


# --------------------------------------------------------------------------- #
# Search and statistics
# --------------------------------------------------------------------------- #

def split(n_queries, search_size, seed):
    perm = np.random.default_rng(seed).permutation(n_queries)
    return perm[:search_size], perm[search_size:]


def run_search(ev, proposer, search_idx, n, rng):
    """The search loop. It only ever reads the search slice of each score."""
    history, seen = [], set()
    for i in range(n):
        cfg = normalize(BASELINE) if i == 0 else proposer.propose(history)
        tries = 0
        while key_of(cfg) in seen and tries < 20:
            cfg, tries = mutate(cfg, rng), tries + 1
        seen.add(key_of(cfg))
        rec, _ = ev.per_query(cfg)
        history.append({"config": cfg, "search_recall10": float(rec[search_idx].mean())})
        print(f"  [{i + 1:>3}/{n}] search R@10 {history[-1]['search_recall10']:.4f}  {key_of(cfg)}")
    return history


def bootstrap_ci(diff, iters=5000, seed=0):
    rng = np.random.default_rng(seed)
    means = diff[rng.integers(0, len(diff), (iters, len(diff)))].mean(1)
    return float(np.percentile(means, 2.5)), float(np.percentile(means, 97.5))


def spearman(a, b):
    ra = np.argsort(np.argsort(a)).astype(float)
    rb = np.argsort(np.argsort(b)).astype(float)
    return float(np.corrcoef(ra, rb)[0, 1])


# --------------------------------------------------------------------------- #
# SVG charts (no plotting dependency)
# --------------------------------------------------------------------------- #

COLORS = ["#2563eb", "#dc2626", "#16a34a", "#9333ea", "#ea580c", "#0891b2"]


def line_chart(path, title, xlabel, ylabel, series, xticks=None, yfmt="{:.3f}"):
    """series: list of dicts with label, xs, ys and optional dashed / step."""
    W, H, L, R, T, B = 760, 420, 64, 190, 44, 52
    xs = [x for s in series for x in s["xs"]]
    ys = [y for s in series for y in s["ys"]]
    x0, x1 = min(xs), max(xs)
    pad = (max(ys) - min(ys)) * 0.08 or 0.01
    y0, y1 = min(ys) - pad, max(ys) + pad
    px = lambda x: L + (x - x0) / ((x1 - x0) or 1) * (W - L - R)
    py = lambda y: H - B - (y - y0) / (y1 - y0) * (H - T - B)
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" '
           f'font-family="system-ui, sans-serif" font-size="12">',
           f'<rect width="{W}" height="{H}" fill="#fff"/>',
           f'<text x="{L}" y="24" font-size="15" font-weight="600" fill="#111">{title}</text>']
    for i in range(6):
        y = y0 + (y1 - y0) * i / 5
        out.append(f'<line x1="{L}" x2="{W - R}" y1="{py(y):.1f}" y2="{py(y):.1f}" stroke="#e5e7eb"/>')
        out.append(f'<text x="{L - 8}" y="{py(y) + 4:.1f}" text-anchor="end" fill="#555">{yfmt.format(y)}</text>')
    ticks = xticks or [(x, str(int(x))) for x in np.linspace(x0, x1, 6)]
    for x, lab in ticks:
        out.append(f'<text x="{px(x):.1f}" y="{H - B + 18}" text-anchor="middle" fill="#555">{lab}</text>')
    out.append(f'<text x="{(L + W - R) / 2}" y="{H - 10}" text-anchor="middle" fill="#333">{xlabel}</text>')
    out.append(f'<text transform="translate(16,{(T + H - B) / 2}) rotate(-90)" text-anchor="middle" '
               f'fill="#333">{ylabel}</text>')
    out.append(f'<line x1="{L}" x2="{L}" y1="{T}" y2="{H - B}" stroke="#999"/>')
    out.append(f'<line x1="{L}" x2="{W - R}" y1="{H - B}" y2="{H - B}" stroke="#999"/>')
    for i, s in enumerate(series):
        c = s.get("color", COLORS[i % len(COLORS)])
        xy = list(zip(s["xs"], s["ys"]))
        if s.get("step"):  # hold each value until the next x
            xy = [p for j, (x, y) in enumerate(xy) for p in ([(x, xy[j - 1][1])] if j else []) + [(x, y)]]
        pts = " ".join(f"{px(x):.1f},{py(y):.1f}" for x, y in xy)
        dash = ' stroke-dasharray="6 4"' if s.get("dashed") else ""
        out.append(f'<polyline points="{pts}" fill="none" stroke="{c}" stroke-width="2"{dash}/>')
        ly = T + 8 + i * 20
        out.append(f'<line x1="{W - R + 14}" x2="{W - R + 36}" y1="{ly}" y2="{ly}" stroke="{c}" '
                   f'stroke-width="2"{dash}/>')
        out.append(f'<text x="{W - R + 42}" y="{ly + 4}" fill="#333">{s["label"]}</text>')
    out.append("</svg>")
    Path(path).write_text("\n".join(out))


# --------------------------------------------------------------------------- #
# Commands
# --------------------------------------------------------------------------- #

def cmd_search(args):
    docs, queries = load_scifact(args.data)
    ev = Evaluator(docs, queries)
    search_idx, held_idx = split(len(queries), args.search_size, args.seed)
    rng = np.random.default_rng(args.seed + 1)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    print(f"{len(docs)} docs, {len(queries)} queries: search {len(search_idx)}, held-out {len(held_idx)}")

    before = None
    if args.proposer == "llm":
        before = gateway_status(args.gateway)
        proposer = LLMProposer(args.gateway, args.model, rng, think=args.think)
    else:
        proposer = RandomProposer(rng)

    t0 = time.time()
    history = run_search(ev, proposer, search_idx, args.candidates, rng)
    wall = time.time() - t0

    # Search is over. Only now are held-out scores read.
    rows = []
    for i, h in enumerate(history):
        rec, ndcg = ev.per_query(h["config"])
        rows.append({**h, "i": i,
                     "held_recall10": float(rec[held_idx].mean()),
                     "search_ndcg10": float(ndcg[search_idx].mean()),
                     "held_ndcg10": float(ndcg[held_idx].mean())})

    s = np.array([r["search_recall10"] for r in rows])
    hd = np.array([r["held_recall10"] for r in rows])
    pick = int(np.argmax(s))
    oracle = int(np.argmax(hd))
    base_rec, _ = ev.per_query(rows[0]["config"])
    pick_rec, _ = ev.per_query(rows[pick]["config"])
    claimed = s[pick] - s[0]
    real = hd[pick] - hd[0]
    ci = bootstrap_ci(pick_rec[held_idx] - base_rec[held_idx])

    plot_curve(out, rows, len(search_idx), len(held_idx), proposer.name)

    summary = {
        "proposer": proposer.name,
        "candidates": len(rows),
        "search_queries": int(len(search_idx)),
        "held_out_queries": int(len(held_idx)),
        "seed": args.seed,
        "baseline": {"search": s[0], "held_out": hd[0]},
        "pick": {"i": pick, "config": rows[pick]["config"], "search": s[pick], "held_out": hd[pick]},
        "claimed_gain": claimed,
        "real_gain": real,
        "real_gain_ci95": ci,
        "optimism": s[pick] - hd[pick],
        "gain_inflation": claimed - real,
        "oracle": {"i": oracle, "held_out": hd[oracle], "search": s[oracle]},
        "spearman_search_vs_held": spearman(s, hd),
        "wall_seconds": wall,
    }
    if args.proposer == "llm":
        after = gateway_status(args.gateway)
        calls = after["calls"] - before["calls"]
        tokens = after["tokens"] - before["tokens"]
        summary["llm"] = {
            "model": args.model,
            "gateway_calls": calls,
            "tokens": tokens,
            "tokens_per_proposal": tokens / max(proposer.calls, 1),
            "gateway_cost_usd": after["cost_usd"] - before["cost_usd"],
            "proposals": proposer.calls,
            "fallbacks": proposer.fallbacks,
            "errors": proposer.errors,
            "mean_latency_s": float(np.mean(proposer.latencies)),
        }

    (out / "results.json").write_text(json.dumps({"summary": summary, "candidates": rows},
                                                 indent=2, default=float))
    (out / "summary.md").write_text(render_search(summary))
    print("\n" + render_search(summary))


def plot_curve(out, rows, n_search, n_held, proposer):
    s = np.array([r["search_recall10"] for r in rows])
    hd = np.array([r["held_recall10"] for r in rows])
    pick_so_far = [int(np.argmax(s[: i + 1])) for i in range(len(s))]
    xs = list(range(1, len(s) + 1))
    line_chart(
        Path(out) / "curve.svg",
        f"Best-of-N on {n_search} search queries vs. {n_held} held-out ({proposer} proposer)",
        "candidates evaluated", "Recall@10",
        [
            {"label": "search score of pick", "xs": xs, "ys": np.maximum.accumulate(s).tolist(), "step": True},
            {"label": "held-out score of pick", "xs": xs, "ys": [hd[j] for j in pick_so_far], "step": True},
            {"label": "best held-out (oracle)", "xs": xs, "ys": np.maximum.accumulate(hd).tolist(),
             "dashed": True, "step": True},
            {"label": "baseline held-out", "xs": [1, len(s)], "ys": [hd[0], hd[0]],
             "dashed": True, "color": "#6b7280"},
        ],
    )


def cmd_replot(args):
    """Redraw a search run's chart and summary from results.json, without re-running it."""
    data = json.loads((Path(args.run) / "results.json").read_text())
    m = data["summary"]
    plot_curve(args.run, data["candidates"], m["search_queries"], m["held_out_queries"], m["proposer"])
    (Path(args.run) / "summary.md").write_text(render_search(m))


def cmd_compare(args):
    """Print a markdown table of claimed vs real gain across search runs."""
    runs = [json.loads((Path(r) / "results.json").read_text())["summary"] for r in args.runs]
    runs.sort(key=lambda m: (m["proposer"], m["seed"]))
    pts = lambda x: f"{x * 100:+.1f}"
    lines = ["| proposer | split seed | claimed gain | real gain (95% CI) | inflation |",
             "|---|---|---|---|---|"]
    for m in runs:
        lo, hi = m["real_gain_ci95"]
        lines.append(f"| {m['proposer']} | {m['seed']} | {pts(m['claimed_gain'])} | "
                     f"{pts(m['real_gain'])} ({pts(lo)} to {pts(hi)}) | {pts(m['gain_inflation'])} |")
    for p in sorted({m["proposer"] for m in runs}):
        ms = [m for m in runs if m["proposer"] == p]
        c = float(np.mean([m["claimed_gain"] for m in ms]))
        r = float(np.mean([m["real_gain"] for m in ms]))
        lines.append(f"| **{p}, mean** | {len(ms)} splits | {pts(c)} | {pts(r)} | {pts(c - r)} |")
    print("\n".join(lines))


def render_search(m):
    pts = lambda x: f"{x * 100:+.1f} pts"
    lines = [
        f"# Search run: {m['proposer']} proposer, {m['candidates']} candidates",
        "",
        f"Search split: {m['search_queries']} queries. Held out: {m['held_out_queries']} "
        f"queries the search never saw. Seed {m['seed']}.",
        "",
        "| | search split | held out |",
        "|---|---|---|",
        f"| Baseline BM25 | {m['baseline']['search']:.4f} | {m['baseline']['held_out']:.4f} |",
        f"| Picked (best on search) | {m['pick']['search']:.4f} | {m['pick']['held_out']:.4f} |",
        f"| Oracle (best on held-out) | {m['oracle']['search']:.4f} | {m['oracle']['held_out']:.4f} |",
        "",
        f"- **Claimed gain** (search split): {pts(m['claimed_gain'])} Recall@10",
        f"- **Real gain** (held out): {pts(m['real_gain'])} "
        f"(95% CI {pts(m['real_gain_ci95'][0])} to {pts(m['real_gain_ci95'][1])})",
        f"- **Gain inflation** (claimed minus real): {pts(m['gain_inflation'])}",
        f"- Optimism of the pick (its search score minus its held-out score): {pts(m['optimism'])}",
        f"- Rank correlation of search vs held-out scores across candidates: "
        f"{m['spearman_search_vs_held']:.2f}",
        f"- Picked config: `{json.dumps(m['pick']['config'], sort_keys=True)}`",
    ]
    if "llm" in m:
        l = m["llm"]
        lines += [
            "",
            f"LLM proposer ({l['model']} via LLMGate): {l['proposals']} proposals, "
            f"{l['gateway_calls']} gateway calls, {l['tokens']:,} tokens "
            f"({l['tokens_per_proposal']:.0f}/proposal), {l['fallbacks']} unparseable replies, "
            f"{l['errors']} gateway errors, {l['mean_latency_s']:.1f}s mean latency.",
        ]
    lines += ["", "![curve](curve.svg)", ""]
    return "\n".join(lines)


def cmd_sweep(args):
    docs, queries = load_scifact(args.data)
    ev = Evaluator(docs, queries)
    rng = np.random.default_rng(args.seed)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    pool, seen = [normalize(BASELINE)], {key_of(normalize(BASELINE))}
    while len(pool) < args.pool:
        cfg = sample(rng)
        if key_of(cfg) not in seen:
            seen.add(key_of(cfg))
            pool.append(cfg)
    # Per-query scores for the whole pool, cached so the analysis can be re-run
    # without re-scoring.
    t0 = time.time()
    cache = out / "scores.npz"
    keys = json.dumps([key_of(c) for c in pool])
    if cache.exists() and str(np.load(cache)["keys"]) == keys:
        R = np.load(cache)["R"]
        print(f"  loaded per-query scores for {len(pool)} configs from {cache}")
    else:
        R = np.zeros((len(pool), len(queries)))
        for i, cfg in enumerate(pool):
            R[i] = ev.per_query(cfg)[0]
            if (i + 1) % 100 == 0:
                print(f"  scored {i + 1}/{len(pool)} configs ({time.time() - t0:.0f}s)", flush=True)
        np.savez_compressed(cache, R=R, keys=keys)

    sizes = [s for s in args.sizes if s < len(queries) // 2]
    budgets = [n for n in args.budgets if n < len(pool)]
    optimism = np.zeros((len(sizes), len(budgets)))
    claimed = np.zeros_like(optimism)
    real = np.zeros_like(optimism)
    false_win = np.zeros_like(optimism)  # search reports a gain the held-out set doesn't
    for a, S in enumerate(sizes):
        for t in range(args.trials):
            search_idx, held_idx = split(len(queries), S, args.seed * 1000 + t)
            s = R[:, search_idx].mean(1)
            h = R[:, held_idx].mean(1)
            order = np.concatenate([[0], 1 + rng.permutation(len(pool) - 1)])  # baseline first
            for c, n in enumerate(budgets):
                cand = order[:n]
                pick = cand[np.argmax(s[cand])]
                optimism[a, c] += s[pick] - h[pick]
                claimed[a, c] += s[pick] - s[0]
                real[a, c] += h[pick] - h[0]
                false_win[a, c] += s[pick] > s[0] and h[pick] <= h[0]
    optimism /= args.trials
    claimed /= args.trials
    real /= args.trials
    false_win /= args.trials

    xt = [(i, str(n)) for i, n in enumerate(budgets)]
    line_chart(
        out / "optimism.svg",
        f"Optimism of the best-of-N pick (avg of {args.trials} random splits)",
        "candidates evaluated (N)", "search minus held-out, Recall@10 points",
        [{"label": f"S = {S} search queries", "xs": list(range(len(budgets))),
          "ys": (optimism[a] * 100).tolist()} for a, S in enumerate(sizes)],
        xticks=xt, yfmt="{:+.1f}",
    )
    summary = {"pool": len(pool), "trials": args.trials, "sizes": sizes, "budgets": budgets,
               "optimism": optimism.tolist(), "claimed_gain": claimed.tolist(),
               "real_gain": real.tolist(), "false_win_rate": false_win.tolist(),
               "seconds": time.time() - t0}
    (out / "sweep.json").write_text(json.dumps(summary, indent=2))
    (out / "summary.md").write_text(render_sweep(summary))
    print("\n" + render_sweep(summary))


def render_sweep(m):
    head = "| search queries (S) | " + " | ".join(f"N={n}" for n in m["budgets"]) + " |"
    sep = "|---|" + "---|" * len(m["budgets"])
    table = lambda grid, fmt="{:+.1f}", scale=100: "\n".join(
        [head, sep] + [f"| {S} | " + " | ".join(fmt.format(v * scale) for v in row) + " |"
                       for S, row in zip(m["sizes"], grid)])
    last = len(m["budgets"]) - 1
    share = "\n".join(
        f"| {S} | {m['claimed_gain'][a][last] * 100:+.1f} | {m['real_gain'][a][last] * 100:+.1f} |"
        for a, S in enumerate(m["sizes"]))
    return "\n".join([
        f"# Sweep: {m['pool']} random configs, {m['trials']} random splits per size",
        "",
        "## Optimism of the pick (search minus held-out Recall@10, points)",
        "",
        table(m["optimism"]),
        "",
        "## False-win rate (search reports a gain; held-out shows none)",
        "",
        table(m["false_win_rate"], fmt="{:.0f}%"),
        "",
        f"## Claimed vs real gain over baseline at N={m['budgets'][last]} (points)",
        "",
        "| search queries (S) | claimed (search) | real (held out) |",
        "|---|---|---|",
        share,
        "",
        "![optimism](optimism.svg)",
        "",
    ])


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--data", default="data/scifact")
    p.add_argument("--seed", type=int, default=7)
    sub = p.add_subparsers(dest="cmd", required=True)

    s = sub.add_parser("search", help="one search run with a full report")
    s.add_argument("--proposer", choices=["llm", "random"], default="llm")
    s.add_argument("--candidates", type=int, default=60)
    s.add_argument("--search-size", type=int, default=100)
    s.add_argument("--gateway", default="http://localhost:8090")
    s.add_argument("--model", default="qwen3:4b")
    s.add_argument("--think", action="store_true", help="let the model reason before answering (slow)")
    s.add_argument("--out", default="results/llm")
    s.set_defaults(fn=cmd_search)

    w = sub.add_parser("sweep", help="optimism vs N and S from a random-search pool")
    w.add_argument("--pool", type=int, default=1000)
    w.add_argument("--trials", type=int, default=200)
    w.add_argument("--sizes", type=int, nargs="+", default=[25, 50, 100, 200, 400])
    w.add_argument("--budgets", type=int, nargs="+", default=[1, 5, 10, 25, 50, 100, 250, 500, 999])
    w.add_argument("--out", default="results/sweep")
    w.set_defaults(fn=cmd_sweep)

    r = sub.add_parser("replot", help="redraw a search run's chart and summary from its results.json")
    r.add_argument("run", help="run directory, e.g. results/llm")
    r.set_defaults(fn=cmd_replot)

    c = sub.add_parser("compare", help="table of claimed vs real gain across search runs")
    c.add_argument("runs", nargs="+", help="run directories")
    c.set_defaults(fn=cmd_compare)

    args = p.parse_args()
    args.fn(args)


if __name__ == "__main__":
    main()
