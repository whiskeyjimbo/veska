# HNSW Backing Library Evaluation Results

Generated: 2026-05-12 07:52:31

## Evaluation Matrix

| Library | Quant | Population | Recall@10 | P95 (ms) | File Size (KB) | Backup Round-Trip | CGo |
|---------|-------|-----------|-----------|----------|---------------|-------------------|-----|
| coder/hnsw | float32 | 50k | 0.7230 | 0.3 | 209692 | N/A | none (pure Go) |
| coder/hnsw | float32 | 250k | 0.6710 | 0.3 | 1045063 | PASS | none (pure Go) |
| lancedb | float32 | 50k | 1.0000 | 58.4 | 150534 | N/A | yes (Rust FFI, liblancedb_go.a) |
| lancedb | float32 | 250k | 1.0000 | 234.4 | 754677 | PASS | yes (Rust FFI, liblancedb_go.a) |
| usearch | float16 | 50k | 0.9860 | 1.8 | 82250 | N/A | yes (C++17, libusearch_c.so) |
| usearch | float16 | 250k | 0.9430 | 4.1 | 411261 | PASS | yes (C++17, libusearch_c.so) |
| usearch | float32 | 50k | 0.9860 | 0.4 | 157250 | N/A | yes (C++17, libusearch_c.so) |
| usearch | float32 | 250k | 0.9430 | 1.1 | 786261 | PASS | yes (C++17, libusearch_c.so) |
| usearch | int8 | 50k | 0.0010 | 2.3 | 44750 | N/A | yes (C++17, libusearch_c.so) |
| usearch | int8 | 250k | 0.0000 | 2.7 | 223761 | PASS | yes (C++17, libusearch_c.so) |

## DoD Criteria

- recall@10 ≥ 0.95 at 50k: check values above
- recall@10 ≥ 0.85 at 250k: check values above
- p95 warm latency ≤ 100ms at k=10 at 250k: check values above
- backup round-trip correctness: PASS/FAIL above

## Notes

- usearch: CGo, requires libusearch_c.so (v2.25.2, installed from .deb). Supports float32/float16/int8 quantization.
- coder/hnsw: pure Go, no CGo, float32 only. File persistence via Export/Import.
- lancedb: CGo via Rust FFI (liblancedb_go.a). Lance columnar directory format. VectorSearch uses brute-force scan (no explicit HNSW index built in-process in v0.1.2).
