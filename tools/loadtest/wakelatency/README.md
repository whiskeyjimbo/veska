# wakelatency - wake-reconcile sweep latency gate

Wall-time gate for `internal/infrastructure/git.WakeReconciler`'s wake
sweep. Generates a synthetic on-disk tree, seeds the reconciler baseline,
then times no-change `InjectWake()` sweeps and asserts the SOLO-03 §5.2 /
`docs/design/13-nfr` wake-reconcile latency NFR:

- **typical repo:** sweep p95 **< 500ms** over N ≥ 20 iterations.
- **>50k files:** a single worst-case sweep **< 5s**.

## What is measured

The sweep cost the NFR targets is the **no-change full walk**: `stat` +
64-byte prefix read + last-seen map compare on every tracked file, with the
handler never firing. After `Seed()` records the baseline, a no-change
`InjectWake()` still walks every file but invokes no handler - that walk is
exactly the cost being gated.

Only `InjectWake()` is timed. Synthetic-tree generation and `Seed()` are
setup and excluded. Git/branch-check and watcher-restart hooks are
deliberately **not** wired - the NFR is about the mtime/size/prefix walk,
which dominates. The handler is an atomic counter asserted to stay `0`, so a
nonzero count fails the run (it would mean the timed sweep included handler
work and the number isn't the pure walk).

A single tree is registered via one `AddDir`; the reconciler recurses into
all subdirs. A single registered tree is one goroutine
(`WithWakeConcurrency` is a no-op), so the 50k walk is single-threaded - the
representative single-repo case.

## Files

| File | Role |
|---|---|
| `wakelatency.go` | Result envelope + JSON writer (no build tag). |
| `wakelatency_test.go` | Sweep timing gate (`//go:build eval`). |
| `wake_latency_results.json` | Latest run output. |

## Running

```sh
make eval-wake-latency
# or directly:
go test -tags eval -run TestWakeLatency ./tools/loadtest/wakelatency/ -v -count=1 -timeout=120s
```

| Env var | Default | Meaning |
|---|---|---|
| `WAKE_FILES` | `5000` | "typical repo" file count for the p95 gate. |
| `WAKE_FILES_LARGE` | `50000` | ">50k files" file count for the worst-case gate. |

The git package needs no `sqlite_fts5` tags, so the target is the bare
`-tags eval` form.

## Synthetic tree

`N` deterministic source-like `.go` files, 100 files per subdir
(`pkg00000/f00000.go`, …). File sizes cycle 1–4KB so the 64-byte prefix
read is representative of real source. Generation is fully deterministic, so
runs are comparable across machines.

## Warm cache

`Seed()` pre-walks the tree, so every timed `InjectWake()` runs against a
warm OS page cache. This is the representative steady-state for a
wake/resume sweep (the working tree was just read at seed time) and is
deterministic - the numbers are warm-cache by design and should not be read
as cold-start. No cold-cache variant is provided.

## Output

JSON envelope (`wake_latency_results.json` next to the bench):

```json
{
  "typical_files": 5000,
  "large_files": 50000,
  "iterations": 25,
  "typical_p95_ms": 12.3,
  "typical_min_ms": 9.8,
  "typical_max_ms": 14.1,
  "large_worst_ms": 110.4,
  "gate_p95_ms": 500,
  "gate_large_ms": 5000,
  "exit_gate_met": true,
  "backend": "git.WakeReconciler",
  "timestamp": "2026-06-01T..."
}
```

Plus a one-line stdout summary:

```
WAKE typical_files=5000 p95_ms=12.30 (min=9.80 max=14.10) large_files=50000 worst_ms=110.40 gate=PASS
```

## Interpreting the numbers

- `typical_p95_ms < 500` → gate (a) met.
- `large_worst_ms < 5000` → gate (b) met.
- `exit_gate_met` is the AND of both.
- On a loaded CI box a p95 within ~20% of the 500ms gate is a watch-out;
  the thresholds are the documented NFR and are not loosened to make a run
  pass.
