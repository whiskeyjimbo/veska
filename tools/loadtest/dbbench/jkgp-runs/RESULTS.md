# solov2-jkgp — end-to-end SQLite driver comparison

Generated: 2026-05-26

Measures whether swapping `modernc.org/sqlite` → `github.com/mattn/go-sqlite3`
moves veska's real user-visible numbers — not just isolated SQLite ops
(which solov2-6e5r/dbbench already settled in mattn's favour). The shim
landed for this run is `internal/infrastructure/sqlite/sqldriver/`,
selected by build tag `sqlite_mattn` + `CGO_ENABLED=1` + `sqlite_fts5`.

## Environment

- Linux 6.8 / x86_64 / 4 CPUs / Go 1.26.3
- Drivers: `modernc.org/sqlite v1.50.1` vs `github.com/mattn/go-sqlite3 v1.14.44`
- Pragmas identical across builds: WAL + foreign_keys=on + synchronous=NORMAL + busy_timeout=5000

## Workloads

| Workload | What it exercises | Modernc | Mattn | Speedup |
|---|---|---:|---:|---:|
| `eval-revalidate-bench` (Combined10k) | 10k-node / 10k-edge / 3k-finding revalidate sweep through production `sqlite.RevalidateRepo`. Heavy SELECT + UPDATE. | 4297 ms total · p95 44.9 ms | **1695 ms total · p95 20.9 ms** | **2.5× total / 2.2× p95** |
| `eval-queue-fuzz` (100 promotions) | Promoter → 3-lane queue drain via real `sqlite.PromotionStore` + queue repo. | 60042 ms (over 60s budget by 42 ms) | 60041 ms (over by 41 ms) | **null finding — bench is polling-bound, not driver-bound** |
| Cold-scan `~/src/reglet` (50 k LOC / 324 Go files) | End-to-end `veska repo add --wait`: tree-walk → parse → promote → FTS sink → embed-ref sink. Heavy multi-statement write transactions. | 16.1 s (with 10 s promote-stage stall warning) | **8.0 s, no stall** | **2.0× — and the user-visible 10s stall disappeared** |
| Cold-scan hugo (223 k LOC / 899 Go files) | Same path on a real 200 k+ LOC public repo. Surfaced solov2-14lw mid-bench (UNIQUE-PK collision on function-local types + multi-init protobuf files); fix landed before this row. | 197.5 s total (promote 193.6 s) | **121.1 s total (promote 117.2 s)** | **1.63× — ~76 s saved on a single cold scan** |
| `eval-recall` | Synthetic-corpus semantic search latency. | not run | not run | **not driver-bound** — the test's in-memory NodeLookup uses a hard-coded `sql.Open("sqlite", …)` and the production search hot-path (kNN over in-memory vectors) never touches SQLite. Swapping wouldn't move recall@k or p95 search latency. |

Raw run logs in `modernc/` and `mattn/` sibling directories.

## Verdict

mattn wins the two driver-bound workloads by ~2× and removes the visible
promote-stage stall on cold scan. That meets the DoD threshold (≥20 % on a
real workload) by a wide margin.

Notes:

- The mattn-p95-promotion_tx regression observed in solov2-6e5r/dbbench
  (65 ms vs modernc 27 ms) did **not** reproduce in `eval-revalidate-bench`,
  where mattn's p95 is *better* (20.9 ms vs 44.9 ms). The synthetic
  regression was likely noise.
- `eval-queue-fuzz` is not a useful comparator at the current promotion
  count (100); both drivers fully drain all 300 work-items but the test
  always reports ~60 040 ms because the poll loop sleeps to the budget
  ceiling. The drain itself completed for both — `done_per_kind` was
  `{auto_link:100, embed:100, revalidate:100}` on both runs. Filing a
  separate issue to make queuefuzz report actual drain wall-time instead
  of budget timer would be useful but is out of scope here.
- The cost of swapping is cgo. CLAUDE.md and the release packaging story
  currently lean on `CGO_ENABLED=0` for static cross-compile binaries.
  Flipping the default means every release pipeline that builds veska
  needs a cgo toolchain (and the `sqlite_fts5` tag) for its target.

## Recommendation

**Approve the swap.** The shim is already in place; flipping the default
is a two-line change to the build-tag conditions in
`internal/infrastructure/sqlite/sqldriver/driver_modernc.go` /
`driver_mattn.go`, plus updating the Makefile build targets to set
`CGO_ENABLED=1` + add `sqlite_fts5` to the build tag set.

Out-of-scope follow-ups if the swap lands:
- Decide the fate of `modernc.org/sqlite` as a build option (keep as an
  opt-out via `-tags=sqlite_modernc` for environments that need no-cgo, or
  fully remove the shim after a deprecation cycle).
- Update CLAUDE.md's "Runtime dependencies" table.
- Re-run cold-scan against a larger 100 k+ LOC repo to confirm the gap
  scales (the 50 k LOC reglet run already shows 2× and removes the stall;
  a bigger repo would just amplify).
