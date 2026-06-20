# Share-vs-Regenerate - ADR-S0019 §4 empirical gate

Generated: 2026-06-02
Platform: linux amd64
Corpus root: `/home/user/src/veska/internal` (max_docs=5000)
Parsed: 315 files, 5318 nodes, 3625 edges, ~1850KB source.
Embedder: **model2vec(potion-code-16M)** - 5000 vecs × 768 dims, embed wall=264ms (mean 53µs/vec).

## Verdict - breakeven bandwidth per artifact

`breakeven = bytes_carried / regen_seconds`: the link speed at which downloading the artifact costs exactly what regenerating it locally does. Below breakeven, regenerate; above, share. No bandwidth is assumed - it is computed.

| Artifact | Decided | Regen | Carried | Breakeven | Note |
|---|---|---|---|---|---|
| parse-derived (nodes/edges/FTS/imports) | REGENERATE | - | - | - | cheap + deterministic (ADR §3). 5318 nodes + 3625 edges parsed in 1.019s; never carried. |
| embeddings (memvec / float32) | MEASURED | 0.26s | 14.6 MB | 58.3 MB/s | model=model2vec(potion-code-16M), 5000 vecs × 768 dims. Below 58.3 MB/s: regenerate; above: share. |
| embeddings (usearch / float16) | MEASURED | 0.26s | 7.3 MB | 29.1 MB/s | model=model2vec(potion-code-16M), 5000 vecs × 768 dims. Below 29.1 MB/s: regenerate; above: share. |
| summaries / condensations | SHARE | - | - | - | LLM-produced → expensive + content-addressable (ADR §3). Measure regen cost with eval-embed-models-condense; verdict is categorical SHARE. |
| review output | SHARE | - | - | - | LLM-produced → expensive + content-addressable (ADR §3). Measure regen cost with eval-review-timing; verdict is categorical SHARE. |
| class-B curation (suppressions / triage) | SHARE | - | - | - | irreproducible, not source-derived (ADR §3) → always share; deterministic fold merges it. |

## Embedding verdict at reference link speeds

Display-only - derived from the breakeven above, not assumed.

| Artifact | 1 Mbit WAN (0.125 MB/s) | 100 Mbit (12.5 MB/s) | 1 Gbit LAN (125 MB/s) | 10 Gbit (1.25e+03 MB/s) |
|---|---|---|---|---|
| embeddings (memvec / float32) | REGEN | REGEN | SHARE | SHARE |
| embeddings (usearch / float16) | REGEN | REGEN | SHARE | SHARE |

## Reading the result

The breakeven scales with per-embed latency: a faster embedder produces a HIGHER breakeven (you would need a faster link for sharing to win → lean regenerate); a slower embedder produces a LOWER breakeven (almost any link beats re-embedding → lean share). The crossover is therefore deployment-dependent - exactly the per-deployment toggle ADR-S0019 §4 places behind `SharedArtifactStore`.

This run measured **model2vec(potion-code-16M)** at 53µs/vec. For the cheap-CPU endpoint, install the fat-build default model2vec (`veska install model2vec`) - sub-millisecond/vec keeps the breakeven high (regenerate up to a fast LAN). For the heavy endpoint, re-run with `VESKA_EMBEDDER=ollama` (network round-trip per vec drives the breakeven toward zero, so share wins at almost any link).

_Measurement basis: regen timed unbatched on a single goroutine; production embeds via a worker pool + `BatchEmbeddingProvider`, so real regen wall-clock is lower and the true breakeven is somewhat HIGHER (biased toward regenerate). The breakeven is corpus-size-invariant - `count` cancels - so a larger library buys measurement stability, not a different verdict._
