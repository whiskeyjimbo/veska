# queuefuzz — M3 gate-5 queue-lane drain fuzz

Drives N synthetic promotions through the real `Promoter` and lets the real
queue `Poller` (with stub `WorkHandler`s) drain every enqueued row across the
three M3 work kinds: `embed`, `auto_link`, `revalidate`.

## What passing means

For every work kind, every enqueued row reached state `done` (or `failed`
with a surfaced error reason) before the budget elapsed. Zero rows remain
`pending` or `in_progress`. The `review` kind is reserved for M5 and is not
exercised here.

## Run

```bash
make eval-queue-fuzz
```

## Knobs

| Env var | Default | Meaning |
|---|---|---|
| `QUEUEFUZZ_PROMOTIONS` | `100` | Number of synthetic single-file promotions to drive. Each enqueues 3 rows (one per kind). |
| `QUEUEFUZZ_BUDGET_MS` | `60000` | Wall-clock budget for the drain. Failing means at least one row remained non-terminal past this point. |
| `QUEUEFUZZ_HANDLER_LATENCY_MS` | `0` | Random per-handler artificial latency (`0..N` ms). Useful to stress concurrency across lanes. |

## Output

JSON is written to stdout (prefixed `QUEUEFUZZ `) and `t.Log`:

```json
{
  "promotions": 100,
  "rows_per_kind":   {"embed": 100, "auto_link": 100, "revalidate": 100},
  "done_per_kind":   {"embed": 100, "auto_link": 100, "revalidate": 100},
  "failed_per_kind": {"embed": 0,   "auto_link": 0,   "revalidate": 0},
  "stuck_per_kind":  {"embed": 0,   "auto_link": 0,   "revalidate": 0},
  "elapsed_ms": 1234,
  "budget_ms":  60000,
  "budget_met": true
}
```
