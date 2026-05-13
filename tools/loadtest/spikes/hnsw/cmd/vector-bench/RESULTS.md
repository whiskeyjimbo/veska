# Application-Layer Bench: UsearchStore (VectorStorage)

Generated: 2026-05-12 22:10:12 UTC

Exercises the production `UsearchStore` via `UpsertEmbeddings` / `Search` (not the raw spike index adapter).
Corpus: 768-dim synthetic vectors, seed=42. Hold-out: 100 queries, seed=999.

## Results

| Population | Recall@10 | P95 (ms) | Recall Floor | P95 Budget | Pass |
|-----------|-----------|----------|-------------|-----------|------|
| 50k | 0.9870 (≥0.95) | 1.90 (≤100) | 0.95 | 100 | ✓ |
| 250k | 0.9540 (≥0.85) | 4.28 (≤150) | 0.85 | 150 | ✓ |

## DoD Floors

- recall@10 ≥ 0.95 @50k
- recall@10 ≥ 0.85 @250k
- p95 ≤ 100ms @50k
- p95 ≤ 150ms @250k
