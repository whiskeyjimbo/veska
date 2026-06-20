# dbbench - SQLite driver comparison

Generated: 2026-05-26T20:22:45Z

## Environment

| Key | Value |
|---|---|
| Go | `go1.26.3` |
| GOOS/GOARCH | `linux/amd64` |
| CPUs | `4` |
| Drivers | `mattn, modernc, zombiezen` |

## Workload config

| Key | Value |
|---|---|
| Seed nodes | 10000 |
| Edges per src | 4 |
| Embedding dim | 768 |
| graph_read iters | 2000 |
| fts_query iters | 1000 |
| queue_poll iters | 1000 |
| promotion_tx iters | 500 |
| bulk_ingest iters × batch | 50 × 500 |
| rehydrate runs | 5 |

## Results

### bulk_ingest

| Driver | p50 ms | p95 ms | p99 ms | max ms | ops/s |
|---|---:|---:|---:|---:|---:|
| mattn | 2.878 | 26.361 | 227.823 | 264.271 | 67.0 |
| modernc | 8.294 | 42.071 | 231.575 | 587.891 | 35.3 |
| zombiezen | 4.314 | 180.649 | 418.097 | 784.971 | 27.0 |

### fts_query

| Driver | p50 ms | p95 ms | p99 ms | max ms | ops/s |
|---|---:|---:|---:|---:|---:|
| mattn | 0.105 | 0.188 | 0.624 | 4.807 | 7501.3 |
| modernc | 0.252 | 0.359 | 0.462 | 0.604 | 3839.7 |
| zombiezen | 0.208 | 0.411 | 1.273 | 2.677 | 4153.0 |

### graph_read

| Driver | p50 ms | p95 ms | p99 ms | max ms | ops/s |
|---|---:|---:|---:|---:|---:|
| mattn | 0.034 | 0.055 | 0.079 | 0.305 | 26937.4 |
| modernc | 0.048 | 0.081 | 0.101 | 0.413 | 19021.6 |
| zombiezen | 0.030 | 0.071 | 0.123 | 0.604 | 26830.8 |

### promotion_tx

| Driver | p50 ms | p95 ms | p99 ms | max ms | ops/s |
|---|---:|---:|---:|---:|---:|
| mattn | 0.387 | 65.712 | 522.237 | 773.962 | 62.9 |
| modernc | 0.566 | 27.392 | 121.562 | 438.289 | 134.5 |
| zombiezen | 0.391 | 52.498 | 293.981 | 993.339 | 72.9 |

### queue_poll

| Driver | p50 ms | p95 ms | p99 ms | max ms | ops/s |
|---|---:|---:|---:|---:|---:|
| mattn | 0.061 | 0.135 | 0.313 | 204.254 | 2084.6 |
| modernc | 0.138 | 0.223 | 0.335 | 96.809 | 2898.2 |
| zombiezen | 0.082 | 0.177 | 0.248 | 350.725 | 748.8 |

### rehydrate_scan

| Driver | p50 ms | p95 ms | p99 ms | max ms | ops/s |
|---|---:|---:|---:|---:|---:|
| mattn | 28.800 | 31.991 | 31.991 | 40.106 | 32.4 |
| modernc | 41.225 | 51.089 | 51.089 | 51.923 | 22.6 |
| zombiezen | 35.751 | 41.850 | 41.850 | 43.813 | 27.2 |

## Verdict

- **bulk_ingest**: fastest = `mattn` (2.878ms p50)
- **fts_query**: fastest = `mattn` (0.105ms p50)
- **graph_read**: fastest = `zombiezen` (0.030ms p50)
- **promotion_tx**: fastest = `mattn` (0.387ms p50)
- **queue_poll**: fastest = `mattn` (0.061ms p50)
- **rehydrate_scan**: fastest = `mattn` (28.800ms p50)

Wins by driver: `mattn`=5 `modernc`=0 `zombiezen`=1 

Recommendation: hand-review the per-workload table; this harness does not auto-pick a winner. If a non-incumbent driver wins ≥4 of 6 workloads with ≥20% p50 improvement on the write paths (promotion_tx, bulk_ingest), file a follow-up to swap. mattn is drop-in; zombiezen needs an adapter rewrite.

