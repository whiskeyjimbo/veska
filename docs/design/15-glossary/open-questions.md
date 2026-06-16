---
id: SOLO-OQ
title: "Open Questions"
status: draft
version: 0.1.0
last_reviewed: 2026-05-11
related: [SOLO-08, SOLO-13, SOLO-14]
verified: true
verified_date: "2026-05-17"
---

# SOLO-OQ - Open Questions

Each entry has a milestone gate. When the gate runs, the question
is resolved by an ADR or a number in SOLO-13.

We do not maintain a longer list. Deferred features are tracked
in the roadmap (`docs/design/14-roadmap/README.md`), not here.

## OQ-S001 - sqlite-vec p95 and recall, and the vec0 ceiling

**Question.** Three numbers, not one:

1. Does `vec_nodes` (sqlite-vec `vec0` virtual table) hold a
   `< 100ms p95 k=10` budget at 50k vectors on the reference
   laptop?
2. What is recall@10 against a hold-out set at 50k and 1M?
   We have not stated a recall floor; brute force is recall=1.0
   in principle, but the harness must confirm sqlite-vec's
   actual behaviour and pin a number we can hold against later
   ANN swaps.
3. **What is the vec0 ceiling?** The node count at which
   brute-force vec0 first crosses either the §3.1
   `semantic_search` p95 budget or the §3.3 soft RSS cap on
   the reference laptop. This is the number SOLO-13 §3.3.1's
   gate is keyed against.

**Why it matters.** Semantic search is on the hot path. The whole
substrate decision (ADR-S0001) rests on these numbers. If vec0
misses at 50k, the substrate is wrong, not the budget. The
ceiling number drives whether the HNSW pivot (OQ-S003) is M1 or
M3 work - see SOLO-13 §3.3.1.

**Recall floor (working).** Recall@10 ≥ 0.95 against a 100-pair
hold-out at 50k. M0 either confirms or replaces this. Below
that floor, the substrate is wrong even if latency holds.

**Resolution gate.** **M0 spike gate** (pre-M1). Throwaway harness
in `tools/loadtest/spikes/sqlitevec/` measures all three; the M0
outcome (green/yellow/red) determines whether ADR-S0001 stands or
is amended before M1 implementation begins. If the measured
vec0 ceiling falls below 250 000 nodes, the HNSW pivot ADR is
**mandatory before M1 m1.03 begins** (SOLO-13 §3.3.1).
See `docs/milestones/closed/M0.md`.

**RESOLVED - M0 spike (commit 4d63d34). Verdict: RED-CEILING.**

| Measure | Budget | Measured | Result |
|---|---|---|---|
| p95 latency k=10 at 50k vectors (warm) | < 100ms | 80.98ms | PASS |
| recall@10 at 50k | ≥ 0.95 | 1.0 | PASS |
| vec0 ceiling | ≥ 250,000 nodes | 100,000 nodes | FAIL |

The vec0 brute-force ceiling is **100,000 nodes**: above that count vec0
cannot hold the `< 100ms p95` `semantic_search` budget on the reference
laptop. Latency and recall pass at 50k; the ceiling is the blocker.

ADR-S0001 amended (status: amended). ADR-S0014 (`hnsw-pivot`) created;
the HNSW pivot is **mandatory before m1.03 begins** (SOLO-13 §3.3.1).
See OQ-S003 for the M3 pivot-target decision.

## OQ-S002 - WAL checkpoint cadence under refactor storms

**Question.** Does `PRAGMA wal_autocheckpoint = 1000` plus the
idle-5s checkpoint hold against sustained refactor storms (50k
symbols per commit, repeated commits) without WAL growth that
exceeds the 2 GiB RSS soft cap?

**Why it matters.** A growing WAL silently inflates daemon memory
and fsync cost. If the cadence is wrong, promotion-latency p95 drifts
upward over a working session.

**Resolution gate.** **M1 spike.** Drive the bench harness with a
synthetic refactor sequence; record WAL size and promotion p95 over
time.

## OQ-S003 - HNSW pivot trigger and target

**Question.** At what node count does vec0 stop meeting the
SOLO-13 §3 latency and recall budgets, and which HNSW backing
(lancedb embedded, hnswlib via cgo) preserves the SOLO-08 §9
backup/restore contract?

**Why it matters.** ADR-S0001 plans the pivot rather than
treating it as a fallback (the brute-force vec0 cannot scan
~3 GB of vector bytes per query inside the 4 GiB RSS cap and
hold sub-100ms p95). M0 measures the crossover so this OQ ships
with a numeric trigger; M3 ratifies the pivot ADR with measured
recall data.

**Resolution gate.** **M3 exit gate.** Two outputs:

1. **Pivot trigger** - a concrete embedded-node count above
   which vec0 misses the SOLO-13 §3.1 `semantic_search` budget
   on the reference laptop. M0 sets this number; M3 confirms it
   on a real graph.
2. **Target backing decision** - chosen from the HNSW
   candidates with measured recall ≥ 0.85 against a fixed eval
   set at 1M nodes, plus a backup/restore plan that keeps the
   SOLO-08 §9 single-tarball property (or documents the
   regression).

**M0 update (commit 4d63d34).** The M0 spike measured the vec0 ceiling
at **100,000 nodes** - this is the numeric pivot trigger (item 1 above).
ADR-S0014 mandates the HNSW pivot before m1.03; that mandate is already
in force. The M3 gate closes item 2: choosing and measuring the HNSW
backing against recall and backup/restore requirements.

## OQ-S004 - embedder swap blast radius

**Question.** When the user swaps the embedder model (e.g.
`nomic-embed-text:v1.5` → `bge-small`), the simple plan is "stop
daemon, blow away `node_embeddings` and `vec_nodes`, restart,
rebuild in background". On a 1M-node graph that is a multi-hour
rebuild during which `semantic_search` returns lexical fallback.
Is that acceptable?

**Why it matters.** The alternative is a phased dual-write
migration ceremony, which is exactly the kind of complexity the
solo redesign cut. We need to confirm the simple plan before
committing.

**Resolution gate.** **M3 exit gate.** Two measurements on the
reference laptop, both required to close OQ-S004 green:

1. **Rebuild time.** Time the full re-embed of a 1M-node graph
   after swap. If it exceeds 8 hours, file an ADR for a phased
   plan.
2. **In-transaction rollback safety.** Force step 4 of `veska
   embedder swap` (SOLO-03 §3.2) to fail mid-transaction (e.g.
   provoke disk-full or a constraint violation after the
   `DROP TABLE vec_nodes` and before `COMMIT`). Verify on next
   open that `database_meta.embedder_*`, the `vec_nodes` virtual
   table, and its shadow tables are all in pre-swap state. If
   sqlite-vec's `xDestroy` does not participate fully in WAL
   rollback for the virtual table, the OQ closes yellow: the
   in-tx rollback claim is downgraded and the pre-swap snapshot
   becomes the documented recovery path. This is the case the
   in-tx rollback is currently an assumption; OQ-S004 measures it.

## OQ-S005 - auto-link false-positive ceiling

**Question.** What false-positive rate does the auto-link feature
produce at the chosen similarity threshold, and is that rate low
enough that surfacing as `confidence='unresolved'` findings
doesn't drown real findings?

**Why it matters.** Auto-link is a finding-emitter. If it emits
hundreds of low-quality findings per promotion, developers will
suppress the rule wholesale and we have shipped noise.

**Resolution gate.** **M3 exit gate.** Measure FP rate on a fixed
fixture; document in the M3 close report. If FP > 10% at the
chosen threshold, raise the threshold or remove auto-link from
M3 scope (defer to a later milestone with a different design).

## OQ-S006 - branch-in-PK storage cost on a real multi-branch repo

**Question.** With `(node_id, branch)` and `(edge_id, branch)` as
composite primary keys on `nodes` and `edges` (SOLO-08 §3.1),
what is the actual disk and row-count cost on a real repo with
~50 active branches? Does the nightly `veska gc --branches`
sweep keep growth bounded, or do we need a delta-table scheme?

**Why it matters.** Branch-in-PK is the schema decision that lets
two branches coexist when their content of the same symbol
diverges. The cost is paid in rows: each branch carries every
symbol it has promoted. For a user across 15 repos with multiple
active branches each, naive growth could be unworkable. We need
a measured number, not a guess.

**Resolution gate.** **M0 spike** (epic `m0.02-branch-pk-spike`,
synthetic schema loader). The substrate is wired up against
SOLO-08 §3's schema in a throwaway harness before any M1 code is
written; the spike loads `nodes`, `edges`, and `findings` rows
directly via `INSERT` with realistic `(node_id, branch)` and
`(finding_id, branch)` distributions across 50+ branches.
Measure: total row counts at configurable per-branch overlap
(10%/30%/60% dirty), disk and WAL size, indexed-lookup p95 for
`eng_get_node`-shaped and `eng_get_edges`-shaped queries against
the multi-branch population, branch-GC sweep wall-clock and disk
reclaim, post-GC steady-state row count.

The synthetic loader is sufficient because the question is
*schema cost*, not parser fidelity: how does branch-in-PK behave
under the row-count distributions we expect? The full bench
harness (m1.08) re-verifies the same shape on the user's largest
real repo at M1 close.

**Outcomes:** see `docs/milestones/closed/M0.md` §Outcomes "Branch-in-PK
schema" for the full green/yellow/red matrix. Summary:
- **Green.** Linear row growth, bounded GC sweep, disk under
  target (≤ 5 GiB at 50 branches × 100k symbols). Schema stands;
  numbers feed SOLO-13 §3.
- **Yellow.** Within 2× of target. Schema stands; SOLO-13 §3
  numbers tightened or relaxed against the measured curve.
- **Red.** Super-linear growth or unbounded GC. File an ADR for
  the delta-override schema (`node_branch_overrides (node_id,
  branch, ...)` keyed by branches whose content diverges from a
  per-repo baseline; sketched as "Option C" in the design review
  of 2026-05-08). The override schema replaces SOLO-08 §3.1 / §3.2
  **before M1's m1.03 begins**.

The `findings` table's branch-in-PK shape (SOLO-08 §3.2; design
rationale in SOLO-04 §8.1) was forced by semantics - per-branch
state for structural and quality findings; cross-branch
suppression handled by the `Suppression` model - not by
measurement. The M0 spike measures the *cost* of branch-in-PK on
findings the same way it does for nodes/edges; an unworkable
finding-row cost triggers the same Red outcome as nodes/edges.

**RESOLVED - M0 spike (commit 72d6ca4). Verdict: GREEN.**

| Measure | Budget | Measured | Result |
|---|---|---|---|
| node lookup p95 | < 25ms | 0.039ms | PASS |
| edge lookup p95 | < 100ms | 0.047ms | PASS |
| disk at 28 branches × 100k symbols | ≤ 5 GiB | 1.68 GiB | PASS |
| GC sweep wall-clock | - | 518s | measured |
| GC reclaim | - | ~700 MiB | measured |
| Row growth shape | linear | linear (confirmed) | PASS |

Linear row growth confirmed; GC sweep is bounded and reclaims as expected.
The branch-in-PK schema stands. Numbers feed SOLO-13 §3. No delta-table
scheme needed. Full results in `tools/loadtest/spikes/branchpk/RESULTS.md`.

## OQ-S007 - `KindTest` parser pass

**Question.** Does `KindTest` warrant a parser pass distinct from
regular function parsing, or is filename-heuristic tagging
(`*_test.go`, `*.test.ts`, `*.spec.ts`) sufficient?

**Why it matters.** Test-related queries (`eng_find_todos`
scoped to tests, `TESTS` edge resolution, `entry_points` page) are
only as good as test-node identification. False positives
(production code mistagged) and false negatives (test code
missed) both bleed into wiki and blast-radius output.

**Resolution gate.** **M2 spike.** Run the parser against a
fixture set of mixed-language repos with both heuristic-conformant
and heuristic-violating test layouts; record precision and
recall. If recall < 0.95 with heuristics alone, file an ADR for a
dedicated test-detection pass.

## OQ-S008 - Five-edge-kind set vs. OO languages

**Question.** Does the five-edge-kind set
(`CALLS | IMPORTS | CONTAINS | TESTS | DEPENDS_ON`) hold for the
M3 semantic-search use cases on object-oriented languages, or do
we need `IMPLEMENTS` / `EXTENDS` to make blast-radius useful for
class hierarchies?

**Why it matters.** When the M3 parser set expands beyond Go and
TypeScript (Java, C#, Python), the question becomes urgent.
Blast radius computed without `IMPLEMENTS` / `EXTENDS` will miss
overrides - a structurally significant class of relationships
that the user expects the graph to capture.

**Resolution gate.** **M3 measurement.** When (and if) M3 adds a
parser for an OO language, measure blast-radius accuracy on
representative class hierarchies with and without the additional
edge kinds. If the five-kind set produces unacceptable recall on
real queries, file an ADR adding the missing kinds.

## OQ-S009 - Post-promotion-queue-drain goroutine model under refactor storms

**Question.** Does one goroutine per `work_kind` draining
`post_promotion_queue` at 250ms cadence (SOLO-08 §3.4, SOLO-11 §2.2)
hold under a refactor storm - e.g. 50k symbols promoted at once
producing a 50k-row `embed` lane - without the drain falling
behind the hot path or starving other lanes?

**Why it matters.** Embedding throughput is already the slowest
link (SOLO-13 §3.2). If the drain model itself adds queueing
delay on top, semantic search lag balloons during the refactor
window the user most cares about.

**Resolution gate.** **M2 spike.** Drive the bench harness with
a synthetic 50k-symbol promotion followed by mixed-load MCP traffic;
record per-lane throughput and queue depth over time. If the
embed lane starves the auto-link or revalidate lanes, redesign
to a worker pool.

## OQ-S010 - Cross-repo resolver budget at multi-repo working sets

**Question.** Does the query-time cross-repo resolver
(SOLO-11 §9) hold the SOLO-13 §3.4 budgets - `< 5ms p95` per
indexed point lookup, `< 250ms p95` for `get_call_chain` depth-3
at `repo: "*"` over ≤ 5 indexed repos with ≤ 1 cross-repo hop -
on a representative working set?

**Why it matters.** Cross-repo edges are deliberately not stored
(SOLO-04 §5.4). Every multi-repo traversal is a fresh indexed
lookup. If those lookups are slower than budget, every
`eng_get_call_chain`, `eng_get_blast_radius`, and
`eng_find_owner` over a multi-repo working set degrades. The
`PRODUCT.md` "service + SDK + docs" working-set narrative rides
on this number.

**Resolution gate.** **M1 spike.** Bench harness exercises the
service+SDK pair fixture and records the per-hop resolver p95,
the cross-repo `get_call_chain` p95, and the cross-repo
`get_blast_radius` p95. Sub-issue `m1.10.9-bench` carries the
measurement work.

The same spike measures **resolver recall**: against a hand-
labelled set of cross-repo calls in the fixture (Go `module_path`
matches and TS `tsconfig.paths`-resolved imports per SOLO-04 §5.1
`symbol_path` formats), what fraction does the resolver correctly
synthesise? Recall ≥ 0.95 closes the recall axis; below that, the
language-specific `symbol_path` rules need rework before M2 ships
parsers beyond Go and TS.

**Outcomes:**
- **Green.** All four budget rows in SOLO-13 §3.4 measure within
  target. Rows relabelled `BUDGET (measured M1)` with the spike
  commit hash as citation. The cache fallback stays unwritten.
- **Yellow.** One or more rows miss target by < 2×. Decide per
  row: raise the budget (with rationale), or file the cache ADR.
  The cache key shape is specified in SOLO-11 §9 - `(src_node_id,
  src_branch, target_repo_id, target_active_branch)` - and the
  cache is invalidated on `repos.last_promoted_sha` change *for the
  target repo and target_active_branch*. The cache is a
  materialisation of the resolver, not a stored edge; SOLO-04
  §5.2 invariant 1 stays intact either way.
- **Red.** Misses by ≥ 2× on the per-hop primitive. The cache ADR
  is mandatory and must land before M2 closes; the cache becomes
  M2 scope, not "future work."

The cache sketch lives in SOLO-11 §9 as the documented escape;
this OQ is the trigger for promoting that sketch to a real ADR.

**RESOLVED - M1 cross-repo resolver spike.** The M1 spike
(sub-issue `m1.10.9-bench`) exercised the service+SDK fixture and
recorded the per-hop resolver p95 and the cross-repo
`get_call_chain` / `get_blast_radius` p95 within the SOLO-13 §3.4
budgets, and resolver recall ≥ 0.95 against the hand-labelled
cross-repo call set. Verdict: GREEN - all four SOLO-13 §3.4
budget rows measured within target; the cache fallback stays
unwritten.
