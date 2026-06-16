# autolink - false-positive eval harness (m3.04.4)

End-to-end FP-rate harness for `internal/application/autolink.Linker`.

The harness drives a real `autolink.Linker` against an in-memory
SQLite (with the real `sqlite.EmbeddingRefsRepo` adapter) plus a real
`VectorStorage` chosen by `vector.NewVectorStorage` (default:
sqlite-vec, per ADR-S0015). The corpus is a deterministic synthetic
dataset of `clusters × nodes_per_cluster` nodes shared with the recall
harness via `tools/loadtest/synthcorpus`: two nodes are "related" iff
they live in the same cluster.

## Files

| File | Role |
|---|---|
| `fp.go` | Pure FP-rate math: `FPRate`, `FPCounts` (no build tag - unit-testable in standard CI). |
| `fp_internal_test.go` | Unit tests for the math above. Runs under default `go test`. |
| `result.go` | JSON envelope + writer. |
| `autolink_test.go` | End-to-end eval test (`//go:build eval`). |

## Running

Quick mode (`pop=1000`, deterministic fake embedder, ~1s):

```sh
make eval-autolink-fp
# or directly:
AUTOLINK_POP=1000 go test -tags=eval -run TestAutolinkFP ./tools/loadtest/autolink/ -v
```

| Env var | Default | Meaning |
|---|---|---|
| `AUTOLINK_POP` | `1000` | Total population. Snapped to a multiple of 100 (clusters). |
| `AUTOLINK_TOPK` | `5` | Per-source candidate cap, passed through `autolink.WithTopK`. |
| `AUTOLINK_THRESHOLD` | `0.85` | Minimum `Hit.Score` for a candidate to be emitted (`autolink.WithThreshold`). Score is in [0,1], higher = more similar. |
| (shared fixture) | - | If `../recall/fixtures/embeddings_<pop>.bin` exists (seeded by the recall harness with `RECALL_GENERATE=1`), this harness replays those vectors instead of using `FakeEmbed`. One generation seeds both gate-2 (recall) and gate-3 (autolink FP). |

## Output

One line to stdout:

```
AUTOLINK_FP pop=1000 fp_rate=0.39 tp=3050 fp=1944 total=4994
```

and a JSON envelope to `autolink_fp_results.json` in the package
directory:

```json
{
  "population": 1000,
  "clusters": 100,
  "nodes_per_cluster": 10,
  "candidates_per_source": 5,
  "threshold": 0.85,
  "fp_rate": 0.39,
  "fp": 1944,
  "tp": 3050,
  "total_candidates": 4994,
  "embedder": "fake",
  "backend": "sqlite-vec",
  "timestamp": "2026-05-14T00:00:00Z"
}
```

## Interpreting the FP rate

`fp_rate = false_positives / total_emitted_candidates`. A pair is a
**true positive** iff the source and target nodes belong to the same
synthetic cluster; otherwise it is a **false positive**. The rate is
bounded in `[0, 1]`; the empty-candidates case is `0` by convention.

The quick-mode number with the fake embedder is **not** an estimate of
production FP rate: the fake embedder is a 64-dim hash projection with
cluster-axis spikes, so any two clusters that share `K mod 64` will
fold onto the same axis and inflate the FP count. Real-Ollama numbers
will replace these once the larger fixtures are seeded (mirroring the
recall harness's milestone-close path).

The harness is most useful as a regression detector: a sudden jump in
quick-mode FP rate on a fixed corpus indicates a behavioural change in
`autolink.Linker` (e.g. threshold tuning, top-k changes, score
direction flip) rather than an embedder change.
