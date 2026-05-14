# recall — semantic-search eval harness (m3.03.3)

End-to-end recall@10 / p95-latency harness for `search.Service.Semantic`.

The harness drives a real `search.Service` (the application-layer
semantic-search orchestrator shipped in m3.03.1) against an in-memory
SQLite `NodeLookupRepo` plus a real `VectorStorage` implementation
chosen by `vector.NewVectorStorage` (default: sqlite-vec, per
ADR-S0015). The corpus is a deterministic synthetic dataset of
`clusters × nodes_per_cluster` nodes with ground truth by construction:
every node in cluster K is a correct hit for cluster K's center query.

## Files

| File | Role |
|---|---|
| `recall.go` | Pure functions: `RecallAtK`, `MeanRecall`, `P95Latency` (no build tag — unit-testable in standard CI). |
| `recall_internal_test.go` | Unit tests for the math above. Runs under default `go test`. |
| `harness.go` | Synthetic corpus generation, deterministic fake embedder, fixture I/O, JSON output envelope. |
| `recall_test.go` | End-to-end eval test (`//go:build eval`). |
| `fixtures/` | On-disk cache of embedding vectors. `*.bin` is gitignored. |

## Running

Quick mode (`pop=1000`, deterministic fake embedder, ~1.2s):

```sh
make eval-recall
# or directly:
RECALL_POP=1000 go test -tags=eval -run TestRecall ./tools/loadtest/recall/ -v
```

Larger populations require a cached embedding fixture at
`fixtures/embeddings_<pop>.bin`. If the fixture is absent and
`RECALL_GENERATE=1` is not set, the test **skips** (not fails) — the
50k/250k full runs are deferred to milestone close.

| Env var | Default | Meaning |
|---|---|---|
| `RECALL_POP` | `1000` | Total population. Snapped to a multiple of 100 (clusters). `≤ 5000` is "quick mode" — fake embedder, no fixture required. |
| `RECALL_GENERATE` | unset | When `1`, allow seeding `fixtures/embeddings_<pop>.bin` if absent. Quick mode persists the fake-generated vectors; large-pop seeding hooks the real-Ollama path. |

## Output

The test prints a single summary line to stdout:

```
RECALL pop=1000 mean_recall=0.65 p95_latency_ms=0.73 embedder=fake backend=sqlite-vec
```

and writes a JSON envelope to `recall_results.json` in the package
directory:

```json
{
  "population": 1000,
  "clusters": 100,
  "nodes_per_cluster": 10,
  "queries": 100,
  "mean_recall": 0.65,
  "p95_latency_ms": 0.73,
  "embedder": "fake",
  "backend": "sqlite-vec",
  "timestamp": "2026-05-14T00:00:00Z"
}
```

## Reproducing the M3 close numbers

1. Seed fixtures from a real Ollama instance (deferred to milestone
   close; the seeding hook lives on the same `RECALL_GENERATE=1`
   switch but is wired against the production
   `ports.EmbeddingProvider` adapter, not the fake embedder).
2. Commit the resulting `fixtures/embeddings_<pop>.bin` artefacts out-
   of-band (they are gitignored on purpose — they're regenerable and
   large).
3. Run `RECALL_POP=50000 make eval-recall` and
   `RECALL_POP=250000 make eval-recall`; the JSON outputs feed the
   M3 close report.

## Quick mode semantics

The fake embedder projects each text to an L2-normalised 64-dim vector
with a strong positive spike on `cluster_id mod 64`. Cluster-center
queries share the spike direction with their members, so a healthy
plumbing path yields mean_recall > 0 (typical: 0.6–0.7 at pop=1000
with sqlite-vec). The bound is intentionally below 1.0: cluster-axis
collisions (when two clusters share `K mod 64`) and the random jitter
prevent perfect separation, which keeps the harness honest about the
search.Service code path rather than the embedder.
