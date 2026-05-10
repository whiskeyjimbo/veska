# sqlite-vec Spike — RESULTS

Generated: 2026-05-10T04:17:58Z

## Verdict

**Outcome bucket:** `red-ceiling`

**Reasons:**

- vec0 ceiling 100000 < required 250000 nodes

## Environment

| Key | Value |
|---|---|
| sqlite-vec version | `v0.1.6` |
| SQLite version | `3.53.0` |
| Platform | `linux/amd64` |

## Latency (warm)

| Population | k | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | gate |
|---|---|---|---|---|---|---|
| 50000 | 10 | 74.15 | 80.98 | 90.34 | 102.44 | PASS (≤ 100ms) |
| 50000 | 50 | 77.20 | 90.50 | 98.57 | 108.04 | — |
| 1000000 | 10 | 1505.09 | 2724.27 | 7662.95 | 25900.80 | RED (> 400ms) |
| 1000000 | 50 | 1530.12 | 2813.65 | 5790.15 | 45069.32 | — |

## Recall

| Population | recall@10 | recall@50 | hold-out | gate |
|---|---|---|---|---|
| 1000000 | N/A (measurement failed) | N/A | N/A | N/A — consistent with ceiling=100k |
| 50000 | 1.0000 | 1.0000 | 100 | PASS (≥ 0.95) |

## vec0 Ceiling

| Metric | Value |
|---|---|
| vec0 ceiling (nodes) | 100000 |
| Ceiling reason | latency |
| Gate (≥ 250000) | FAIL

## Resource Usage (RSS / Disk)

| Population | Load wall time | Disk | Peak RSS |
|---|---|---|---|
| 50000 | 10974ms | 148.3 MiB | 305.7 MiB |
| 1000000 | 256011ms | 2.89 GiB | 4.90 GiB |

## Measurement Notes

> **NOTE:** 1M recall: measurement failed — sqlite-vec returned an internal error when inserting ~272k vectors into the 1M recall DB. This is consistent with the vec0 ceiling of 100k nodes detected in the bench sweep.

## Exit-Gate Summary

| Gate | Measured | Threshold | Result |
|---|---|---|---|
| 50k warm p95 | 80.98ms | ≤ 100ms | PASS |
| 50k recall@10 | 1.0000 | ≥ 0.95 | PASS |
| 1M warm p95 | 2724.27ms | ≤ 200ms (green) / ≤ 400ms (yellow) | RED |
| 1M recall@10 | N/A (failed) | ≥ 0.85 (green) / ≥ 0.75 (yellow) | N/A |
| vec0 ceiling | 100000 | ≥ 250000 | FAIL |

