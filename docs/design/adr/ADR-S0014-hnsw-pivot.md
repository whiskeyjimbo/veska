---
id: ADR-S0014
title: HNSW vector index pivot (mandatory M1 pre-requisite, replaces vec0 above 100k nodes)
status: accepted
date: 2026-05-11
deciders: [whiskeyjimbo]
supersedes: []
related: [ADR-S0001, OQ-S003]
---

# ADR-S0014 — HNSW Vector Index Pivot

## Context

ADR-S0001 adopted sqlite-vec (`vec0`) as the vector substrate with a staged-adoption
caveat: if the M0 spike found the vec0 ceiling below 250k embedded nodes, the HNSW
pivot ADR (OQ-S003) would be promoted from M3 to M1 mandatory work.

M0 measured the ceiling at **100k nodes** on a Linux VM (spike commit `4d63d34`).
The measurement was subsequently re-verified on the reference laptop (M2 MacBook Air)
with the same result: p95 crosses 100ms between 100k and 150k nodes regardless of
SQLite tuning (mmap, page cache, page size from 4KB to 64KB all tested — no meaningful
difference). The bottleneck is arithmetic throughput on the brute-force L2 scan, not
I/O. The ceiling is confirmed at ~100k–125k on reference hardware, well below the
250k minimum. The pivot is mandatory.

## Decision

Replace `vec0` with an HNSW-backed index for all vector queries at or above 100k
embedded nodes. The chosen backing is **to be determined** in m1.03 based on:

1. **Recall floor:** recall@10 ≥ 0.95 at 50k; ≥ 0.85 at 1M.
2. **Latency budget:** warm p95 ≤ 100ms at k=10 across the full working range.
3. **Backup story:** must be embeddable in the single `~/.engram/` tarball or
   `VACUUM INTO` snapshot without requiring external processes.
4. **Candidate libraries:** `lancedb` (embedded HNSW, Go bindings), `hnswlib` via
   cgo, or sqlite-vec's own HNSW when available.

## Status

This ADR is **accepted** as the decision to pivot. The implementation ADR (choosing
the specific HNSW library and schema) is owned by epic m1.03.

## Consequences

- M1's m1.03 may not begin until the implementation choice is ratified.
- ADR-S0001's `status` changes from `proposed` to `amended`.
- SOLO-13 §3.3.1's vec0 ceiling is updated to the measured 100k number.
- OQ-S003 is resolved: the HNSW pivot is no longer a future contingency but the
  committed path.
