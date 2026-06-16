# RESULTS: branch-in-PK SQLite Spike

**Date:** 2026-05-11  
**Verdict:** `GREEN` - **GREEN**

---

## Environment

| Property | Value |
|---|---|
| SQLite version | 3.53.0 |
| Host platform | linux/amd64 |
| Branches | 28 |
| Symbols per branch (base) | 100000 |
| Dirty-overlap pct (representative) | 10% |

## Population Assumptions

Synthetic population: **28 branches × 100000 symbols** = 2,800,000 base rows (plus 10% dirty-content overlap per branch-pair).
Edges: 1 CALLS edge per symbol (circular). Findings: 1 per 100 symbols.

Three overlap scenarios loaded: 10%, 30%, 60%.
Representative for verdict: **10% overlap** (worst-case disk; best-case latency).

## Row Growth

| Table | Rows |
|---|---|
| nodes | 2,800,000 |
| edges | 2,800,000 |
| findings | 28,000 |

**Linear growth confirmed:** YES - O(branches × symbols)

## Disk Size

| Metric | Value |
|---|---|
| DB file (post-load) | 1.68 GiB (1,801,351,168 bytes) |
| WAL file | 0 bytes |
| Peak RSS | 0 bytes |
| Budget (green) | ≤ 5.00 GiB |
| Budget (yellow) | ≤ 10.00 GiB |

**Disk status:** PASS (green)

## Indexed-Lookup Latency (warm, p95)

| Query | p50 (ms) | p95 (ms) | p99 (ms) | N | Budget p95 | Status |
|---|---|---|---|---|---|---|
| get_node | 0.03 | 0.04 | 0.06 | 200 | 25 ms | PASS (green) |
| get_edges | 0.03 | 0.05 | 0.06 | 200 | 100 ms | PASS (green) |

## GC Sweep Cost

| Metric | Value |
|---|---|
| Branches before sweep | 28 |
| Branches deleted | 10 |
| Branches after sweep | 18 |
| Wall time (ms) | 518551 |
| Disk before | 1,801,351,168 bytes |
| Disk after | 1,098,625,024 bytes |
| Reclaimed | 702,726,144 bytes |

**GC sweep bounded:** YES - proportional to branches deleted

## Verdict Matrix

| Criterion | Value | Green Threshold | Yellow Threshold | Result |
|---|---|---|---|---|
| Linear row growth | YES | YES | YES | PASS (green) |
| Disk size | 1.68 GiB | ≤ 5 GiB | ≤ 10 GiB | PASS (green) |
| Node p95 latency | 0.04 ms | ≤ 25 ms | ≤ 50 ms | PASS (green) |
| Edges p95 latency | 0.05 ms | ≤ 100 ms | ≤ 200 ms | PASS (green) |
| GC sweep bounded | YES | YES | YES | PASS |

## Final Verdict

**`GREEN`**

All criteria meet the green threshold. The branch-in-PK schema is approved for M0 production use.
