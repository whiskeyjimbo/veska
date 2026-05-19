# recallprojection — embed-text projection recall sweep (solov2-7ma)

Makes embed-text **projection** changes measurable for recall sweeps.

## Why this exists

The collapse-correctness fix folded `file_path` + `language` into the
production `FetchPending` embed-text projection (`domain.EmbedText`). The
open question is whether to ALSO fold the symbol **signature** and/or a
code **snippet** into the embed text for semantically richer embeddings.
That changes embedding QUALITY, so it needs a recall measurement.

The existing `tools/loadtest/recall` harness embeds the synthetic corpus
`Text` field directly — it does **not** exercise `domain.EmbedText`, so
swapping projection variants there does not move the measured recall.

This harness closes the gap: the recall corpus is built from
**node-shaped projection inputs** run through `domain.EmbedText` — the
same function the production sqlite adapter calls — and the projection
variant is selectable, so a variant change is exactly what the recall
delta measures.

## Projection variants

`domain.EmbedTextVariant`, selected per run:

| Variant | Embed text |
|---|---|
| `baseline` | `<kind> <symbol_path> <file_path> <language>` (production) |
| `+signature` | baseline + the symbol signature |
| `+snippet` | baseline + a code snippet |
| `+both` | baseline + signature + snippet |

Enrichment variants only **append** — `baseline` is always a prefix, so
production output is never altered by this harness.

## Files

| File | Role |
|---|---|
| `projection.go` | Build-tag-free: corpus builder, variant selector. Unit-testable in standard CI. |
| `projection_test.go` | Unit tests for the above. Runs under default `go test`. |
| `projection_eval_test.go` | End-to-end Ollama-backed sweep (`//go:build eval`). |

## Running the sweep

```sh
make eval-recall-projection
# or directly:
RECALL_POP=1000 go test -tags=eval -run TestRecallProjectionSweep \
  ./tools/loadtest/recallprojection/ -v -timeout=3600s
```

| Env var | Default | Meaning |
|---|---|---|
| `RECALL_POP` | `1000` | Total population; snapped to a multiple of the semantic cluster count. |
| `RECALL_PROJECTION_VARIANT` | unset | Restrict to one variant (`baseline` / `+signature` / `+snippet` / `+both`). Unset sweeps all four. |
| `VESKA_OLLAMA_URL` | `http://localhost:11434` | Ollama base URL (probe + embedding). |
| `VESKA_EMBED_MODEL` | `nomic-embed-text` | Embedding model. |
| `VESKA_VECTOR_BACKEND` | `sqlite-vec` | Vector backend. |

If Ollama is unreachable the test **skips** (it does not fail), matching
the sibling `recall` and `embedder` harnesses.

## Output

One summary line per variant:

```
RECALL_PROJECTION variant=baseline pop=1000 mean_recall=0.84 p95_latency_ms=42.10 embedder=ollama:nomic-embed-text backend=sqlite-vec
```

plus a JSON array at `recall_projection_results.json`.

## Reference-laptop run (steps 2 + 3, deferred)

A full 4-variant sweep at `pop=1000` issues ~4×1000 real embed calls and
is slow — run it on the reference laptop, not in CI. Compare the
`mean_recall` per variant against the m3.03 baseline; the variant with
the best recall (without an unacceptable p95 regression) is the candidate
to promote into the production projection. Promotion is a separate change
to `internal/infrastructure/sqlite/embedding_refs_repo.go` (`embedText`)
and is **out of scope** for the harness task.
