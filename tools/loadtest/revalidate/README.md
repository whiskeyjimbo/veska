# revalidate - 10k-edge commit timing bench (m3.05.4)

End-to-end wall-time bench for `internal/application/revalidate.Handler`.
Drives a synthetic 10 000-node / 10 000-edge / ~3 000-finding commit
through the production sqlite `RevalidateRepo` and asserts the M3
exit-gate target: **full sweep must finish in < 60 s**.

## Files

| File | Role |
|---|---|
| `revalidate.go` | Result envelope + JSON writer (no build tag). |
| `revalidate_test.go` | End-to-end timing bench (`//go:build eval`). |
| `revalidate_bench_results.json` | Latest run output (committed only when re-baselining). |

## Running

```sh
make eval-revalidate-bench
# or directly:
go test -tags=eval -run TestRevalidateBench ./tools/loadtest/revalidate/ -v -timeout=120s
```

| Env var | Default | Meaning |
|---|---|---|
| `REVALIDATE_NODES` | `10000` | Total node count for the `Combined10k` sub-test. Must be a multiple of `REVALIDATE_FILES`. |
| `REVALIDATE_FILES` | `100` | File count. Each file gets `NODES/FILES` nodes plus that many within-file edges. |
| `REVALIDATE_STALE_PCT` | `30` | Percentage of nodes-per-file that anchor a stale open finding (split half dead-code / half contract-drift). |

There is **no quick-mode override** - the M3 exit gate is specifically
the 10k-edge case, so the bench always runs the full fixture.

## Sub-tests

The single `TestRevalidateBench` invocation runs three sub-tests so a
regression in one dispatch path surfaces with its own signal:

| Sub-test | Fixture | Purpose |
|---|---|---|
| `DeadCodeOnly` | 4 files × 100 nodes, 20% dead-code anchors | Isolates the `HasInboundEdges` path. |
| `ContractDriftOnly` | 4 files × 100 nodes, 20% drift anchors | Isolates the `NodeSignaturePair` path. |
| `Combined10k` | 100 files × 100 nodes, 30% mixed anchors | The actual M3 gate. |

The gate is **only** asserted on `Combined10k`. The two isolation
sub-tests still fail on any handler error or refresh-vs-close count
drift (DoD #7) but have no wall-time assertion of their own.

## Fixture shape

- Nodes per file are partitioned into three contiguous ranges:
  - `[0, deadEnd)` - dead-code anchors. The edge layout guarantees
    these have zero inbound edges, so dispatch takes the REFRESH
    branch.
  - `[deadEnd, driftEnd)` - contract-drift anchors. Their
    `prev_signature` differs from `signature`, so dispatch also takes
    REFRESH.
  - `[driftEnd, nodesPerFile)` - "callee tail". Every edge dst lives
    here, which keeps the dead-code heads inbound-free without
    requiring per-node edge logic.
- Findings have `anchor_content_hash = 'h-stale-<node_id>'`; nodes have
  `content_hash = 'h-current-<node_id>'`. The hash mismatch is what
  makes the row "stale" for `StaleFindingsForFile`.

## Output

JSON envelope (`revalidate_bench_results.json` next to the bench):

```json
{
  "nodes": 10000,
  "files": 100,
  "edges": 10000,
  "findings_total": 3000,
  "findings_stale": 3000,
  "refreshed": 3000,
  "closed": 0,
  "elapsed_ms": 1234.5,
  "p95_handle_ms": 18.7,
  "exit_gate_met": true,
  "backend": "sqlite",
  "timestamp": "2026-05-14T..."
}
```

Plus a one-line stdout summary per sub-test:

```
REVALIDATE[combined] nodes=10000 files=100 findings=3000 elapsed_ms=1234.50 p95_ms=18.70 refreshed=3000 closed=0 gate=PASS
```

`elapsed_ms` covers only the per-file `Handle` loop - fixture seeding is
excluded so the number is comparable across runs and machines.

## Interpreting the numbers

- `elapsed_ms < 60000` → M3 exit gate met.
- `refreshed + closed == findings_total` → DoD #7 sanity. The default
  fixture is constructed so every finding refreshes (no callers ever
  appear on dead-code heads; signature pair always drifts). A non-zero
  `closed` count on the default fixture indicates a fixture-seeding
  bug, not a dispatch bug.
- `p95_handle_ms` is the per-file latency at the 95th percentile. With
  the default fixture (100 nodes × 30 findings per file) this typically
  sits in the low-tens-of-ms; a sharp regression here usually points at
  index loss on `findings(state)` or the join in
  `StaleFindingsForFile`.
