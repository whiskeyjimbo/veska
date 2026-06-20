# vec0-to-HNSW Pivot: ANN Recall & Latency Summary

Generated: 2026-05-14

This file summarizes the M1 benchmark results from `tools/loadtest/spikes/hnsw/`
and `tools/loadtest/spikes/hnsw/cmd/vector-bench/` that inform ADR-S0014
(vec0-to-HNSW pivot and dual-backend strategy).

## Spike results (raw library, float16)

Source: `tools/loadtest/spikes/hnsw/RESULTS.md` (generated 2026-05-12).

| Backend   | Quant   | Population | Recall@10 | P95 (ms) | File (KB) | Backup round-trip |
|-----------|---------|-----------|-----------|----------|-----------|-------------------|
| usearch   | float16 | 50k       | **0.9860** | 1.8      | 82,250    | N/A               |
| usearch   | float16 | 250k      | **0.9430** | 4.1      | 411,261   | PASS              |
| coder/hnsw| float32 | 50k       | 0.7230    | 0.3      | 209,692   | N/A               |
| coder/hnsw| float32 | 250k      | 0.6710    | 0.3      | 1,045,063 | PASS              |
| lancedb   | float32 | 250k      | 1.0000    | 234.4    | 754,677   | PASS              |

## Production-layer bench (UsearchStore via VectorStorage port, float16)

Source: `tools/loadtest/spikes/hnsw/cmd/vector-bench/RESULTS.md` (generated 2026-05-12).
Corpus: 768-dim synthetic vectors seed=42. Hold-out: 100 queries seed=999.

| Population | Recall@10 | P95 (ms) | Recall floor | P95 budget | Pass |
|-----------|-----------|----------|--------------|------------|------|
| 50k       | **0.9870** | 1.90    | ≥0.95        | ≤100ms     | ✓    |
| 250k      | **0.9540** | 4.28    | ≥0.85        | ≤150ms     | ✓    |

## DoD floor assessment

| Criterion                        | Result    | Status |
|----------------------------------|-----------|--------|
| recall@10 ≥ 0.95 @50k (usearch)  | 0.9870    | ✓ PASS |
| recall@10 ≥ 0.85 @250k (usearch) | 0.9540    | ✓ PASS |
| p95 ≤ 100ms @50k                 | 1.90ms    | ✓ PASS |
| p95 ≤ 150ms @250k                | 4.28ms    | ✓ PASS |
| backup round-trip @250k          | PASS      | ✓ PASS |

## Dual-backend verdict

| Backend    | Use case                                           | Deps                        |
|------------|----------------------------------------------------|-----------------------------|
| sqlite-vec | Default; zero external libs; linear scan ≤75k nodes| CGo (sqlite-vec.c embedded) |
| usearch    | Scale; HNSW float16; recall@10=0.987 @50k          | libusearch_c.so + hnsw_native tag |

- **Default backend**: `sqlite-vec` - no `libusearch_c.so` required; adequate
  below `SQLiteVecYellowThreshold` (75k nodes).
- **Scale backend**: `usearch` (float16, HNSW) - selected library from M1 spike;
  all DoD floors pass at both 50k and 250k populations.
- vec0 impl removed; sqlite-vec covers the low-count case without the vec0 ceiling.

## sqlite-vec ceiling thresholds

Tracked in `internal/infrastructure/vector/sqlitevec` constants:

| Threshold | Value | veska doctor storage behavior |
|-----------|-------|-------------------------------|
| Yellow    | 75k   | Warning: approaching sqlite-vec ceiling |
| Red       | 90k   | Error: switch to usearch backend         |
