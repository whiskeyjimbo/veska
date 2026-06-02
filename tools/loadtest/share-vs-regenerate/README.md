# share-vs-regenerate — ADR-S0019 §4 empirical gate

Measures, per derived-artifact family, the **local regeneration cost** against
the **size/sync cost** of carrying that artifact in the shared store, and places
the share-vs-regenerate line *from data* rather than assumption.

This is a **measurement harness**, not a pass/fail gate. It times the existing
pipeline stages (parse via the production `GoParser`, embed via the production
elected embedder) on a real library and sizes the resulting artifacts.

## The verdict is a breakeven bandwidth

For each artifact:

```
breakeven = bytes_carried / regen_seconds
```

— the link speed at which downloading the artifact costs exactly what
regenerating it locally does. **Below breakeven: regenerate. Above: share.**
No bandwidth is hardcoded into the verdict; reference link speeds (WAN / 100 Mbit
/ LAN / 10 Gbit) are layered on top purely for display.

> **Measurement basis.** Regen is timed unbatched on a single goroutine, whereas
> production embeds via a worker pool + `BatchEmbeddingProvider` — so real regen
> wall-clock is lower and the true breakeven is somewhat *higher* (biased toward
> regenerate). The breakeven is corpus-size-invariant (`count` cancels), so a
> larger library buys measurement *stability*, not a different verdict.

## Why only embeddings are "measured"

ADR-S0019 §3 already decided four of the five families *qualitatively*:

| Family | Verdict | Why |
|---|---|---|
| parse-derived (nodes/edges/FTS/imports) | **regenerate** | cheap + deterministic |
| summaries / condensations | **share** | LLM-produced → expensive + content-addressable |
| review output | **share** | LLM-produced → expensive + content-addressable |
| class-B curation | **share** | irreproducible, not source-derived |
| **embeddings** | **measured** | deployment-dependent (§5) — the only gray-zone family |

So the harness measures embeddings empirically (against both vector-backend
widths: memvec float32 and usearch float16) and reports the other families'
categorical verdicts with pointers to their own timing harnesses
(`eval-embed-models-condense`, `eval-review-timing`).

## Run

```bash
make eval-share-vs-regenerate                         # this repo's internal/ self-corpus
SHARE_REGEN_ROOT=/path/to/kubernetes \
  make eval-share-vs-regenerate                       # a genuinely large repo
VESKA_EMBEDDER=ollama make eval-share-vs-regenerate   # heavy-embedder crossover endpoint
```

Runs with **no external service**: the elected embedder is model2vec when
installed, else the zero-dependency static-v2 hash embedder. Install model2vec
(`veska install model2vec`) for the fast-CPU endpoint of the crossover; point
`VESKA_EMBEDDER=ollama` (with Ollama up) for the heavy endpoint.

## Knobs

| Env var | Default | Meaning |
|---|---|---|
| `SHARE_REGEN_ROOT` | this repo's `internal/` | corpus root to scan (any Go tree) |
| `SHARE_REGEN_MAX_DOCS` | `5000` | cap on embedded nodes (bounds runtime) |
| `VESKA_EMBEDDER` | auto | `model2vec` \| `static` \| `ollama` (forwarded to `elect`) |
| `VESKA_OLLAMA_URL` | `http://localhost:11434` | Ollama base URL (ollama branch) |
| `VESKA_EMBED_MODEL` | `nomic-embed-text` | Ollama embedding model (ollama branch) |

## Output

- `RESULTS.md` (next to this README) — the per-artifact table + the embedding
  verdict at reference link speeds + a data-driven reading.
- A `SHARE_REGEN ` JSON line on stdout (the same rows, machine-readable).
