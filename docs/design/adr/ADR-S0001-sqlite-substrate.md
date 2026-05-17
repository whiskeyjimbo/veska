---
id: ADR-S0001
title: SQLite + sqlite-vec as the V2.0 substrate (staged adoption)
status: amended
date: 2026-05-08
amended_date: 2026-05-11
deciders: [whiskeyjimbo]
verified: true
verified_date: "2026-05-16"
superseded_by: [ADR-S0014, ADR-S0015]
---

> **Vector-substrate update (post-amendment).** The HNSW pivot
> anticipated below is now recorded: **ADR-S0014** ratifies the
> pivot and **ADR-S0015** adopts the shipped dual-backend strategy
> (sqlite-vec by default, usearch/float16 HNSW above the
> M2-ratified threshold). The vector-storage portion of this ADR
> is superseded by S0014/S0015; the graph-substrate decision
> (one SQLite file, WAL, atomic promotion) stands unchanged.
> Note: S0014 moved the pivot ADR to a mandatory M1 pre-requisite,
> ahead of the M2 placement described in the staged-adoption note
> below — see S0014 for the current schedule of record.

> **Staged adoption.** sqlite-vec is the V2.0 substrate up to
> ~100k embedded nodes (the M0 / M1 working range). Above that,
> a pivot to an HNSW-backed vector index (lancedb or equivalent,
> kept inside the same SQLite-rooted on-disk layout where
> possible) is **expected, not contingent** — the brute-force
> `vec0` index does not have headroom to ~1M vectors at the
> latency budgets in SOLO-13 §3 on the reference laptop. **M0
> measures the actual crossover; the pivot ADR is written and
> ratified at M2** (epic `m2.06-vec-pivot-adr` — see
> `milestones/M2.md`), so M3 and later milestones build on the
> pivoted index rather than treating it as optional. The prior
> framing that placed the pivot ADR at M3 lagged behind the
> arithmetic in SOLO-13 §3.3.1 and the "service + SDK + docs"
> working-set claim in PRODUCT.md; the M2 placement closes that
> gap. This ADR moves to `accepted` once the M0 measurement
> confirms the working-range claim and the M2 pivot ADR is
> recorded against measured numbers.

# ADR-S0001 — SQLite + sqlite-vec as the V2.0 substrate

## Context

Engram Solo is one daemon, one developer, one machine. The prior V2
design called for Dolt sql-server (graph) plus Qdrant (vectors), each
as a supervised child process, with one Dolt branch per Git branch.
That topology was sized for a server tier we are not building.

What the solo product actually needs from storage:

- Hold ~1M nodes and ~5M edges for a 100k-LOC repo.
- Hold ~1M float32[768] embeddings (~3 GiB raw).
- Atomic `INSERT ... COMMIT` for the promotion transaction.
- ANN search at the budget in SOLO-13 §3.1 (`semantic_search` row;
  unmeasured at write time, gated by the M0 spike).
- Survive `kill -9` without corrupting the database.
- Be backed up by an online snapshot mechanism that doesn't
  require quiescing writers (SOLO-08 §9 — `VACUUM INTO` plus a
  tarball of supporting files).

A single SQLite file with the `sqlite-vec` extension covers every
bullet above, in one process, with no child supervision and no
network ports. WAL mode gives us non-blocking reads under a writing
promotion. `sqlite-vec`'s `vec0` virtual table handles cosine ANN at the
scales we care about.

The tradeoff we accept: we lose Dolt's time-travel queries
(`AS OF`), branch-level diffs, and the "graph as Git" mental model.
For a single-user product none of those features have a present
consumer. Time-travel for graphs gets re-derived from Git anyway —
we already have a promoted graph per commit, indexed by `git_sha`.

## Decision

The substrate is one SQLite database file at `~/.veska/veska.db`,
opened with WAL mode and the `sqlite-vec` extension loaded. Schema is
documented in SOLO-08; the canonical version lives in
`internal/infrastructure/sqlite/migrations/`.

Vector embeddings live in the same file via `vec_nodes` (a `vec0`
virtual table). Embedding bytes are content-addressed in
`node_embeddings`; per-node refs live in `node_embedding_refs`.

There is no Dolt. There are no Dolt branches mirroring Git branches.
There is no Qdrant. There is no `_workspace` sentinel database. The
`branch` value is a column on the relevant tables (see ADR-S0007
companion text in SOLO-08 §4).

## Consequences

Positive:

- Zero child processes for storage. The daemon is one Go binary.
- Backup is `tar`. Restore is `tar -x` and restart. Reset is `rm -rf`.
- One SQL transaction wraps the promotion: graph delta + post-promotion queue row, atomic.
- `WAL` mode means readers do not block the writer.
- No port allocation, no health probes for storage, no version-skew
  matrix between Dolt and Qdrant.

Negative:

- Lose Dolt time-travel. Mitigation: per-commit promoted state is
  already keyed by `git_sha` in `post_promotion_queue`; "the graph as of SHA"
  is reconstructable from the current promoted state plus Git diff.
  Where this is insufficient, file an ADR.
- SQLite has one writer at a time. The hot path is the promotion
  transaction; concurrent MCP write tools queue. At solo scale this
  is a feature, not a problem.
- Vector recall ceiling. `sqlite-vec`'s `vec0` table is
  brute-force; a k=10 query at 1M × 768-dim vectors scans roughly
  3 GB of float bytes per call. Sub-100 ms p95 at that scale
  requires the entire vector table to live in OS page cache,
  which collides with the 4 GiB RSS hard cap (SOLO-13 §3.3).
  vec0 covers the M0 / M1 working range (≤ ~100k embedded
  nodes); the HNSW pivot (lancedb or equivalent) lands at M2
  with the M0 measurement in hand. The pivot trigger is the
  measurement, not a hunch.

Open questions tracked in SOLO-OQ:

- OQ-S001: vec0 latency / recall at 50k–100k vectors on the
  reference laptop. Resolution: **M0 spike gate** (pre-M1).
  Sets the upper bound of vec0's working range.
- OQ-S002: WAL checkpoint cadence under sustained refactor storms.
  Resolution: M1 spike.
- OQ-S003: HNSW pivot ADR. Resolution: **M2 epic
  `m2.06-vec-pivot-adr`** (not M3 — see staged-adoption note
  above). The M2 ADR records the
  measured failure mode of vec0 at scale and the on-disk layout
  that keeps backup/restore (SOLO-08 §9) intact through the
  swap. M3 builds on the pivoted index.

## Alternatives Considered

- **Dolt single-branch (no branch-per-Git-branch).** Still a child
  process, still a 200+ MB binary, still its own port and supervisor.
  All the operational cost, none of the time-travel benefit because
  we collapsed the branches.
- **BoltDB / BadgerDB.** Embedded KV stores, but no SQL. We would
  re-implement indices, joins, and the query planner. Net cost
  higher than SQLite for our query shape.
- **Postgres (embedded or local).** Order of magnitude more
  operational surface than SQLite for a single-user product.
  `pgvector` is a fine vector index but the host process is overkill.
- **LanceDB only (no SQL).** Excellent vectors, no relational story.
  We would still need SQLite for nodes/edges and then run two stores.
- **Qdrant for vectors + SQLite for graph.** Two stores, two failure
  modes, two backups, two version skew matrices. The
  one-file-on-disk property is worth more than Qdrant's marginal
  recall advantage at our scale.

## M0 Measurement (2026-05-11)

**Verdict: red-ceiling** — spike commit `4d63d34`

| Gate | Measured | Threshold | Result |
|---|---|---|---|
| 50k warm p95 (k=10) | 80.98 ms | ≤ 100 ms | PASS |
| 50k recall@10 | 1.0000 | ≥ 0.95 | PASS |
| 1M warm p95 (k=10) | 2724.27 ms | ≤ 200 ms | RED |
| vec0 ceiling | 100 000 nodes | ≥ 250 000 | RED-CEILING |

**Platform:** sqlite-vec v0.1.6 / SQLite 3.53.0 / linux/amd64

**Interpretation.** The 50k working range is fine (p95=81ms, perfect recall). The
brute-force `vec0` index cannot reach 250k nodes within the SOLO-13 §3.1 latency
budget on the reference laptop. The measured ceiling at 100k is the input
SOLO-13 §3.3.1's gate is keyed against.

**Action per M0 §Outcomes red-ceiling row:**
1. This ADR is amended (status → `amended`). The staged-adoption note in the preamble
   remains accurate: vec0 covers the M0/M1 working range up to ~100k nodes (not ~100k
   as originally estimated — the measured ceiling is exactly at the lower bound).
2. OQ-S003 (HNSW pivot ADR) is promoted from M3 to **mandatory M1 work**, to be
   written and ratified **before m1.03 begins**. The pivot ADR will specify the chosen
   HNSW backing (lancedb or equivalent), the recall floor, and how it preserves the
   single-tarball backup story.
3. SOLO-13 §3.3.1 is updated with the measured ceiling (100k nodes).
4. M1's m1.03 must not begin until the pivot ADR is accepted.
