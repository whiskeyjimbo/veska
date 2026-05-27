# tokenefficiency — tokens-saved-vs-grep+read benchmark (solov2-wise)

Produces a defensible "tokens saved vs grep+read" figure for veska
search, paired with recall on the same corpus so the savings number
can't be gamed by returning nothing.

This harness is **separate** from `internal/savings`. Do not conflate
them in user-facing output:

- `internal/savings` — live char-ratio telemetry per real query (ops).
- `tokenefficiency` — offline benchmark with a recall anchor (docs).

## Running

```sh
make eval-token-efficiency
# or directly:
go test -tags=eval -run TestTokenEfficiency ./tools/loadtest/tokenefficiency/ -v
```

Output (single-line summary on stdout + JSON envelope at
`tools/loadtest/tokenefficiency/results.json`):

```
Veska found the right code ~98% of the time, using about 42% as many tokens as grep+read would have (range: 58–58% fewer, depending on how aggressively the agent reads grep matches; measured on 30 queries).
~ savings: 246 tokens/query · 12,300 tokens over 50 searches · $0.0369 at $3/Mtok (Claude Sonnet input).
TOKENEFF embedder=model2vec:potion-code-16M queries=30 recall=0.98 veska_tok=176 grep_lo=422 grep_hi=422 savings=[58%, 58%]
```

The second line denominates the same data in concrete units — useful
when quoting in a budget or docs blurb. Tunable knobs for the
extrapolation:

| Env var | Default | Meaning |
|---|---|---|
| `TOKEFF_CONVERSATION_QUERIES` | `50` | Assumed searches per agent conversation; multiplier on the per-query savings. |
| `TOKEFF_USD_PER_MTOKEN` | `3.0` | $ per million input tokens. Defaults to Claude Sonnet input rate. |
| `TOKEFF_PRICE_LABEL` | `Claude Sonnet input` | Human label printed alongside the $ figure so a stale rate is obvious. |

The harness prefers the in-process **model2vec** embedder
(`potion-code-16M`, distilled from CodeRankEmbed) when its assets are
present at `internal/infrastructure/embedding/model2vec/assets/`. If the
assets are missing it falls back to the deterministic FakeEmbedder
(hash-projection; recall ~17%, plumbing-only). Run `make build` or
`make fetch-embed-assets` once to populate the assets. The embedder
name is printed in every summary so the recall number is never quoted
without context.

Tunable knobs:

| Env var | Default | Meaning |
|---|---|---|
| `TOKEFF_NODES_PER_CLUSTER` | `24` | Members per cluster in the synthetic semantic corpus. Larger files exaggerate grep's read-all-matches cost so the savings range widens. Capped by `synthcorpus.semanticNodesPerClusterCap` (792). |
| `TOKEFF_REPOS` | `5` | Multi-repo harness only. Partitions clusters across N synthetic repos so veska's cross-repo fanout + global RRF (solov2-bcn) competes against a grep that walks every repo's filesystem. |

## Multi-repo (solov2-kcmo)

The wedge pitch is "your agent searches across the whole workspace".
The multi-repo harness measures exactly that:

```sh
make eval-token-efficiency-multirepo
# or directly:
go test -tags=eval -run TestTokenEfficiencyMultiRepo ./tools/loadtest/tokenefficiency/ -v
```

Layout: clusters are split contiguously across `TOKEFF_REPOS` repos.
For each query, veska runs `search.Service.SemanticCandidates` against
every repo and fuses with a single global RRF (mirroring the
production MCP handler shipped in solov2-bcn). The simulated grep
walks every repo's filesystem.

Output: `results-multirepo.json` (same envelope as the single-repo
result plus a `repos` field) and a one-line summary:

```
Across 5 repos, veska found the right code ~100% of the time, using about 42% as many tokens as grep+read across the same workspace (range: 58–58% fewer; measured on 30 queries).
```

Honest caveats specific to the multi-repo run:

- The synthetic corpus has zero cross-cluster phrase overlap, so the
  simulated grep matches exactly one file per query (the cluster's
  own file) regardless of how many repos exist. That keeps the
  baseline tokens nearly identical to the single-repo run — savings
  ratios reported here are conservative. A real workspace with
  repeated identifiers across repos would multiply grep's read cost
  and widen the savings bracket.
- Multi-repo recall matches (and on this corpus exceeds) single-repo
  thanks to **cosine fusion** in the cross-repo path (solov2-uuuk):
  when one embedder spans every repo, raw cosine scores are
  comparable across repos, so the global fanout picks the actually-
  best match instead of tying every repo's rank-1 candidate. The
  earlier RRF-only path was capped near ~49% on the same corpus.

## Methodology

- **Corpus.** `tools/loadtest/synthcorpus.GenerateSemanticCorpus` — one
  hand-authored topic vocabulary per cluster. Each node = 5 phrases
  drawn (without repetition) from the cluster's 12-phrase bag. Center
  queries are the first 5 phrases of each topic so grep can actually
  match the corpus.
- **Tokenizer.** [tiktoken-go](https://github.com/pkoukk/tiktoken-go)'s
  `cl100k_base` encoding. The encoding choice doesn't bias the ratio
  (veska and grep get the same tokenizer) — it only matters for
  cross-tool comparisons.
- **Veska side.** Runs `search.Service.Semantic` (real
  application-layer service against an in-memory SQLite + sqlite-vec
  backend with the deterministic `FakeEmbedder`). Tokens =
  `tiktoken(concat(top-K snippets))`.
- **grep+read baselines.** Pure-Go simulation: for each query, the
  matching file set is `{f : any query phrase is a substring of f}`.
  Two bracketed read strategies:
  - **stop-when-covered (lower bound on savings):** walk grep hits in
    sorted order, accumulate tokens, stop once a read file contains
    any truth node. Lower-bound recall is binary per query.
  - **read-all-matches (upper bound on savings):** read every grep hit.
- **Recall.** Standard `recall@K` against the cluster-construction
  ground truth.

## Honest caveats (printed in `corpus_note` of `results.json`)

- The fixture is **auto-generated** from the indexed graph. Ground
  truth is by construction (every cluster member counts as a hit for
  that cluster's center query). This biases recall up vs human-
  annotated corpora like semble's `benchmarks/annotations/`. Importing
  those annotations for overlapping repos is a deliberate
  out-of-scope follow-up.
- The simulated `grep` matches on phrase substrings — a real `rg`
  with regex / word boundaries would produce a different file set on
  some queries. The bracket (`SavingsLo`, `SavingsHi`) is intended to
  absorb that variance; future work could replay live `rg` output for
  a sample of queries.
- The default `FakeEmbedder` is hash-based, not a real model.
  Recall numbers will look low; that's expected and HONEST — the
  savings figure has a recall anchor printed alongside it so the
  number cannot be quoted in isolation.

## Output schema

```json
{
  "queries":               30,
  "k":                     10,
  "tokenizer":             "cl100k_base",
  "mean_recall":           0.17,
  "mean_veska_tokens":     177,
  "mean_grep_lo_tokens":   422,
  "mean_grep_hi_tokens":   422,
  "mean_savings_lo_vs_grep": 0.58,
  "mean_savings_hi_vs_grep": 0.58,
  "mean_grep_lo_recall":   1.0,
  "per_query": [...],
  "corpus_note":           "...",
  "timestamp":             "2026-05-27T..."
}
```
