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

1. **Recall floor:** recall@10 ≥ 0.95 at 50k; ≥ 0.85 at 250k.
2. **Latency budget:** warm p95 ≤ 100ms at k=10 at 250k vectors on the reference
   laptop.
3. **Backup round-trip:** `Save` → tar into `engram backup create` → `Load` must
   reproduce identical query results. Measured with 5 hold-out queries before and
   after round-trip. Index file size at 250k recorded for float32, float16, and
   int8 quantization (usearch supports all three; record recall delta per level).
4. **Candidate libraries:**
   - `usearch` (Unum Cloud, cgo, C++17, float32/float16/int8, mmap persistence)
   - `coder/hnsw` (pure Go, no cgo, float32 only, file persistence)
   - `lancedb` (embedded columnar + HNSW, Go SDK, Lance format)
   - sqlite-vec HNSW when available (deferred — no release date)

## Status

This ADR is **accepted** as the decision to pivot. The implementation ADR (choosing
the specific HNSW library and schema) is owned by epic m1.03.

## Implementation

Evaluated 2026-05-12 via spike at `tools/loadtest/spikes/hnsw/` (commit recorded in
this ADR). Full results in `tools/loadtest/spikes/hnsw/RESULTS.md`.

### Evaluation Results

| Library | Quant | Pop | Recall@10 | P95 (ms) | File (KB) | Round-Trip | CGo |
|---------|-------|-----|-----------|----------|-----------|------------|-----|
| usearch | float32 | 50k | **0.986** | **0.4** | 157,250 | N/A | yes (C++17) |
| usearch | float32 | 250k | **0.943** | **1.1** | 786,261 | PASS | yes (C++17) |
| usearch | float16 | 50k | **0.986** | 1.8 | 82,250 | N/A | yes (C++17) |
| usearch | float16 | 250k | **0.943** | 4.1 | 411,261 | PASS | yes (C++17) |
| usearch | int8 | 50k | 0.001 | 2.3 | 44,750 | N/A | yes (C++17) |
| usearch | int8 | 250k | 0.000 | 2.7 | 223,761 | PASS | yes (C++17) |
| coder/hnsw | float32 | 50k | 0.723 | 0.3 | 209,692 | N/A | none |
| coder/hnsw | float32 | 250k | 0.671 | 0.3 | 1,045,063 | PASS | none |
| lancedb | float32 | 50k | 1.000 | 58.4 | 150,534 | N/A | yes (Rust FFI) |
| lancedb | float32 | 250k | 1.000 | 234.4 | 754,677 | PASS | yes (Rust FFI) |

DoD thresholds: recall@10 ≥ 0.95 @50k, ≥ 0.85 @250k; p95 ≤ 100ms @250k.

### Analysis

**usearch (float32/float16)** is the only candidate that passes all three DoD gates:
- recall@10 = 0.986 @50k (floor 0.95 ✓) and 0.943 @250k (floor 0.85 ✓)
- p95 = 1.1ms @250k (budget 100ms ✓)
- Backup round-trip: PASS
- CGo dependency: requires `libusearch_c.so` (v2.25.2 .deb) and `usearch.h`

**float16 quantization** halves file size (786→411 MB at 250k) with identical recall —
use float16 as the default, float32 as the precision fallback. int8 is unsuitable for
768-dim float32 embeddings (near-zero recall).

**coder/hnsw** fails the recall floor at both sizes (0.723 @50k, 0.671 @250k). Pure Go
is attractive for build simplicity, but M=16/EfSearch=100 is insufficient at 768 dims
with cosine-like vectors. The library lacks quantization. Excluded.

**lancedb** achieves perfect recall (brute-force scan under the hood — v0.1.2 does not
build an explicit HNSW index in-process). p95 = 234ms @250k is 2× above the 100ms
budget. Heavy Rust FFI dependency with no binary in the module cache (needs download +
ranlib). Excluded.

### Selected Library

**usearch** (`github.com/unum-cloud/usearch/golang`, v2.25.2 native library) with
**float16 quantization** as the default index format.

Both usearch-float32 and usearch-float16 are within noise on recall and latency, so
both are considered "top two" per the spike plan. The float16 prototype is the
production target (smaller files, identical recall); float32 is the fallback precision
mode. A single prototype covers both since the same adapter supports both quantization
levels via the `Quantization` enum.

Build requirement: install `usearch_linux_amd64_2.25.2.deb` from USearch releases, or
link against `libusearch_c.so` at compile time. CGo is required.

## Consequences

- M1's m1.03 may not begin until the implementation choice is ratified.
- ADR-S0001's `status` changes from `proposed` to `amended`.
- SOLO-13 §3.3.1's vec0 ceiling is updated to the measured 100k number.
- OQ-S003 is resolved: the HNSW pivot is no longer a future contingency but the
  committed path.
- usearch v2.25.2 is a CGo dependency. CI must install the .deb or link the .so.
- int8 quantization is excluded for 768-dim nomic-embed-text vectors (near-zero recall).
- coder/hnsw and lancedb are excluded; rationale documented above.
