# Multi-Branch Bench - M1 Gates 4+5 + OQ-S006

Generated: 2026-05-13
Platform: linux amd64

## Phase 1 - Steady-State Seed (50 branches × 5000 nodes)

| Metric | Value |
|--------|-------|
| Branches seeded | 50 |
| Nodes per branch | 5000 |
| Total node rows | 250000 |
| Total edge rows | 250000 |
| Total finding rows | 2500 |
| DB file size (post-seed) | 129.4 MiB |

## Phase 2 - Promotion Trials (20 trials × 50000 nodes)

| Metric | Value |
|--------|-------|
| Trials | 20 |
| Nodes per trial | 50000 |
| Promo p50 | 3.27s |
| Promo p95 | 3.83s |
| Total node rows (post-promo) | 1250000 |
| Total edge rows (post-promo) | 1250000 |
| DB file size (post-promo) | 647.4 MiB |

## Phase 3 - Query p95 (OQ-S006)

| Metric | Value |
|--------|-------|
| Iterations | 200 |
| Query p50 | 0.022ms |
| Query p95 | 0.030ms |
| Query p99 | 0.035ms |

## Phase 4 - GC Sweep (10 branches deleted)

| Metric | Value |
|--------|-------|
| Disk before GC | 647.4 MiB |
| GC sweep time | 7.265s |
| Disk after GC (post-VACUUM) | 598.7 MiB |
| Reclaimed | 48.7 MiB |

## OQ-S006 Comparison vs M0

M0 baseline: 28 branches × 100k nodes, disk=1.68 GiB, node-query p95=0.04ms

| Ratio | M0 | M1 | Ratio | Threshold |
|-------|----|----|-------|-----------|
| Rows/branch | 100000 | 5000 | 0.05x | <2x |
| Disk/row (bytes) | 644.2 | 542.7 | 0.84x | <2x |
| Query p95 (ms) | 0.040 | 0.030 | 0.75x | <2x |

OQ-S006 verdict: **GREEN** - all ratios < 2x vs M0 baseline

## Gate Results

| Metric | Value | Budget | Verdict |
|--------|-------|--------|---------|
| Daemon RSS | 17mb | ≤2GiB | PASS |
| Promotion 50k p95 | 3.83s | <5s | PASS |
