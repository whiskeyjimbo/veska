---
id: ADR-S0014-vec-pivot-dual-backend
title: "vec0-to-HNSW pivot and dual-backend vector strategy"
status: Accepted
date: 2026-05-14
deciders: [whiskeyjimbo]
supersedes: []
related: [ADR-S0014, ADR-S0001, ADR-S0002]
---

# ADR-S0014-vec-pivot-dual-backend: vec0-to-HNSW pivot and dual-backend vector strategy

## Status

Accepted

## Context

ADR-S0014 committed veska to an HNSW pivot after M0 measured the vec0 (sqlite-vec
brute-force scan) ceiling at 100k nodes on the reference hardware. The pivot ADR left
the specific backing library as TBD, to be resolved by the m1.03 evaluation spike.

M1 evaluated four candidates — usearch, coder/hnsw, lancedb, and vec0 itself — across
recall, latency, backup round-trip, and dependency weight. Results are in
`tools/loadtest/spikes/hnsw/RESULTS.md` and `tools/loadtest/spikes/hnsw/cmd/vector-bench/RESULTS.md`.

### Why a dual-backend design

After the M1 evaluation, two distinct user profiles emerged:

1. **Small workspaces (< 75k nodes)**: The brute-force linear scan is fast enough
   (< 1ms), the sqlite-vec CGo dependency compiles from source with no external .so,
   and the vectors live in the same `veska.db` SQLite file that the rest of veska
   already writes. Zero new infrastructure.

2. **Large workspaces (≥ 75k nodes)**: usearch HNSW with float16 quantization
   delivers recall@10 = 0.987 @50k and 0.954 @250k at p95 = 1.9ms and 4.3ms
   respectively — well within all DoD floors. The trade-off is a compiled
   `libusearch_c.so` dependency and the `hnsw_native` build tag.

Making sqlite-vec the **default** removes the CGo shared-library install step from
the new-user journey entirely. Users who grow past 75k nodes opt into usearch by
switching a config key.

## Decision

### Scale backend: usearch (float16, HNSW)

Selected from M1 evaluation as the only candidate passing all DoD gates:

| Criterion                        | Measured   | Floor   | Pass |
|----------------------------------|------------|---------|------|
| recall@10 @50k  (production layer)| 0.9870    | ≥0.95   | ✓    |
| recall@10 @250k (production layer)| 0.9540    | ≥0.85   | ✓    |
| p95 @50k                          | 1.90ms    | ≤100ms  | ✓    |
| p95 @250k                         | 4.28ms    | ≤150ms  | ✓    |
| backup round-trip @250k           | PASS      | PASS    | ✓    |

Configuration: L2sq metric, float16 quantization, connectivity=16,
expansion_add=64, expansion_search=100. Index files: `vec-{repo}|{branch}|{model}.hnsw`
+ JSON sidecar in VESKA_HOME.

### Default backend: sqlite-vec (linear scan)

Zero extra native dependencies beyond standard CGo. Adequate for workspaces below
75k embedded nodes. Vectors are stored in the `vec_embeddings` table inside `veska.db`
(same database as the graph store), so the existing backup round-trip covers them.

Implementation: `internal/infrastructure/vector/sqlitevec` — pure in-memory
map + brute-force L2 scan. Clearly documented as "dev/low-count, linear scan".

### Config key

```
vector.backend: sqlite-vec | usearch   (default: sqlite-vec)
```

Factory: `internal/infrastructure/vector.NewVectorStorage(kind BackendKind, dir string)`.

### Recall floor (committed)

For the usearch backend:
- recall@10 ≥ 0.95 @50k nodes
- recall@10 ≥ 0.85 @250k nodes

Measured against the fixed eval set: 768-dim synthetic vectors seed=42,
100 hold-out queries seed=999.

### vec0 removal

The vec0 (sqlite-vec virtual table `vec0`) implementation is not retained.
sqlite-vec's linear-scan table covers the low-count case with simpler code and
no vec0 virtual-table overhead. The transition window for M2 uses sqlite-vec
linear scan as the default; usearch is opt-in.

## Backup property

Both backends produce files that are included in the `veska backup create` tarball:

| Backend    | File(s) in tarball                          | Mechanism                          |
|------------|---------------------------------------------|------------------------------------|
| sqlite-vec | `veska.db` (contains `vec_embeddings` table)| VACUUM INTO in backup.Create       |
| usearch    | `vec-*.hnsw` + `vec-*.json` in VESKA_HOME   | To be included by backup.Create extension (tracked) |

The usearch index files (`.hnsw` + `.json`) currently live at the top level of
VESKA_HOME. The `backup.Create` function copies `veska.db`, `audit.jsonl`, and
`cache/`. Extending backup.Create to walk and include `vec-*.hnsw` files is tracked
as a follow-up; in the interim, operators can place the usearch indexes under `cache/`.

## Ceiling warnings (sqlite-vec backend)

`veska doctor storage` surfaces backend, vector count, and ceiling warnings:

| Threshold | Constant                      | Warning level |
|-----------|-------------------------------|---------------|
| ≥ 75,000  | SQLiteVecYellowThreshold      | yellow        |
| > 90,000  | SQLiteVecRedThreshold         | red           |

Implemented in `internal/doctor/storage_backend.go` via `CheckStorageBackend`.
No ceiling warnings are emitted for the usearch backend.

## Excluded candidates

- **coder/hnsw**: recall@10 = 0.723 @50k (floor 0.95). Pure-Go build is attractive
  but recall is insufficient for 768-dim nomic-embed-text vectors. Excluded.
- **lancedb**: p95 = 234ms @250k (budget 100ms). Brute-force scan in v0.1.2; heavy
  Rust FFI. Excluded.
- **usearch int8**: recall@10 ≈ 0.001 @50k. Near-zero recall for 768-dim float32
  embeddings. Excluded.

## Consequences

- New installs default to sqlite-vec: no `libusearch_c.so` installation required.
- Users growing past 75k nodes see `veska doctor storage` yellow warning, prompting
  `vector.backend: usearch` in config.
- Migration path from sqlite-vec to usearch: `veska vector migrate` (planned M3).
- usearch users must install the `libusearch_c.so` shared library and build with
  `-tags hnsw_native`.
- CI default (no `-tags hnsw_native`) uses sqlite-vec; all tests pass without
  native HNSW libraries.
