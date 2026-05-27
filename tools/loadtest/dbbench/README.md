# dbbench — Go SQLite driver comparison (solov2-6e5r)

Benchmarks the three viable Go SQLite drivers against the workloads veska's
storage layer actually runs, to decide whether the production driver (the
pure-Go `modernc.org/sqlite`) should be swapped.

## Drivers

| Driver | Build tags | Notes |
|---|---|---|
| `modernc.org/sqlite` | `eval` | Production default. Pure-Go, no cgo. |
| `mattn/go-sqlite3`   | `eval,cgo,sqlite_fts5` | cgo. `sqlite_fts5` is mandatory — the bench's FTS5 workload (`fts_query`) needs it. Drop-in replacement at the `database/sql` driver name level. |
| `zombiezen.com/go/sqlite` | `eval` | Pure-Go. Non-`database/sql` API; the bench has a parallel implementation in `driver_zombiezen.go`. Swap would require an adapter rewrite of `internal/infrastructure/sqlite/*`. |

## Workloads

Each workload is run after `WarmupIters` warmup iterations; p50/p95/p99/max
are computed from per-iteration wall time, ops/s from total elapsed time.

| Name | What it models | veska prod path |
|---|---|---|
| `graph_read` | `SELECT` node by id + fan-out of outbound edges | `internal/infrastructure/sqlite/node_lookup_repo.go`, `edge_reader_repo.go` |
| `fts_query` | Two MATCHes (words + trigram) against fts5 tables | `internal/infrastructure/sqlite/lexical_repo.go` |
| `queue_poll` | Tx that picks the next pending queue row and marks it done | `internal/infrastructure/sqlite/queue/*` |
| `promotion_tx` | Multi-stmt write tx: 10 node UPDATEs, 10 ref UPDATEs, 1 queue INSERT | `internal/application/promoter.go` + `sqlite.PromotionStore` |
| `bulk_ingest` | One tx with N node INSERTs (default 500) | initial ingest path |
| `rehydrate_scan` | Full scan of `node_embedding_refs JOIN node_embeddings` (blob read) | `internal/application/embedder/rehydrate.go` (runs every daemon start) |

The schema lives under `schema/0001_core.sql` — a trimmed superset of the
production `internal/infrastructure/sqlite/migrations` (just what the six
workloads touch). The bench owns its own schema so it can be applied
identically through both the `database/sql` drivers and zombiezen's `sqlitex`.

## Running

```sh
# All registered drivers (modernc + zombiezen by default; pure-Go).
make eval-dbbench

# Include mattn (requires cgo).
make eval-dbbench-cgo

# Or directly:
go test -tags=eval        -run TestDBBench -timeout=600s -v ./tools/loadtest/dbbench/
CGO_ENABLED=1 \
go test -tags="eval cgo sqlite_fts5" -run TestDBBench -timeout=600s -v ./tools/loadtest/dbbench/
```

| Env var | Default | Meaning |
|---|---|---|
| `DBBENCH_DRIVERS` | all registered | Comma list (e.g. `modernc,mattn`). |
| `DBBENCH_NODES` | `10000` | Seed node count. Edges scale linearly. |
| `DBBENCH_DB` | (unset) | Path to an existing veska.db. If set, the harness copies it to a tempdir, skips schema+seed, and runs the workloads against it. Schema must be compatible with the bench's queries — only modernc-built veska.db files have been tested. |
| `DBBENCH_QUICK` | (unset) | Slash iter counts ~10× for a smoke run. |
| `DBBENCH_OUT` | `RESULTS.md` | Output path for the report. |

## Interpreting

`RESULTS.md` is regenerated on every run. The verdict block at the end picks
the fastest driver per workload and counts wins, but **does not auto-pick a
winner** — driver swap decisions need a human eye on the write-path numbers
(`promotion_tx`, `bulk_ingest`) and the operational cost of taking cgo (or
rewriting adapters, for zombiezen).
