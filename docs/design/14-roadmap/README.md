---
id: SOLO-14
title: "Roadmap & Milestones"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-01, SOLO-08, SOLO-11, SOLO-13]
---

# SOLO-14 — Roadmap

Seven milestones. **M0 is a substrate-spike milestone** that ships
no production code; it validates sqlite-vec (OQ-S001) and the
branch-in-PK schema (OQ-S006) with throwaway synthetic loaders
before any M1 work begins. M1–M5 build the product; M6 cuts the
docs over and archives the prior tree. There is no separate
scale-spike milestone; the measurements that the prior plan
slotted into M2.5 are folded into M0 (substrate-cost spikes) and
M1 (real-repo re-verification), where they belong.

## Shape of a milestone

Each milestone has:

- **Goal** — one sentence.
- **Epics** — 4–6, each with a 2-line DoD.
- **Exit gates** — measurable conditions. Numbers cite the spike,
  not a wish.
- **Dependencies** — earlier milestones whose exit gates must hold.

The detail per milestone (epic decomposition, sub-issues, beads
IDs) lives under [`milestones/`](../../milestones/).

---

## M0 — Substrate spikes (no production code)

**Goal:** Empirically validate the two substrate decisions M1
will be built on top of — sqlite-vec as the vector index
(ADR-S0001 / OQ-S001) and branch-in-PK as the row-key shape
(SOLO-08 §4 / OQ-S006) — before any production code is written.
Two throwaway harnesses; two decisions; numbers feed SOLO-13 §3.

| Epic | DoD |
|---|---|
| **m0.01 — vec0 spike** | Synthetic 768-dim vector population at 50k and 1M; warm/cold p95, recall@10/@50, RSS, on-disk size. Outcome bucket determines whether ADR-S0001 stands. |
| **m0.02 — branch-in-PK schema spike** | Synthetic `nodes`/`edges`/`findings` loader at 50 branches × 100k symbols with configurable per-branch overlap; row counts, disk + WAL, indexed-lookup p95, branch-GC sweep cost. Outcome bucket determines whether SOLO-08 §3.1/§3.2's branch-in-PK shape stands. |
| **m0.03 — decisions and amendments** | Update ADR-S0001, SOLO-08 §3/§4, SOLO-13 §3, OQ-S001, OQ-S006 per the two outcomes. Confirm M1 plan. |

**Exit gates:** see [`milestones/M0.md`](../../milestones/M0.md)
§Outcomes for the green/yellow/red matrices on each spike.

**Dependencies:** none. M0 is the entry gate to M1.

---

## M1 — Substrate foundation

**Goal:** A layered V2 codebase with SQLite + sqlite-vec storage,
the save/promote split, fsnotify watcher, tree-sitter parsers for Go
and TypeScript, basic MCP (10 tools), `engram doctor`, and an
append-only audit log.

| Epic | DoD |
|---|---|
| **m1.01 — scaffold** | `engram-v2/` packages declared per SOLO-07; `make build test lint` green; `layercheck` analyser in place. |
| **m1.02 — domain & ports** | `Node`, `Edge`, `Graph`, `Task`, `Finding`, `Suppression`, `Actor` with functional-options constructors; ports in `core/ports/`. |
| **m1.03 — sqlite substrate** | Schema per SOLO-08 §3 migrated; sqlite-vec loaded; `post_promotion_queue` poller running. Daemon refuses to start without sqlite-vec. |
| **m1.04 — save/promote pipeline** | Staging in-memory; post-commit hook promotions; post-promotion queue drained by goroutines; hook return < 100ms p95 (measured). |
| **m1.05 — parsers & watcher** | Tree-sitter Go + TypeScript; fsnotify watcher; cold-scan 100k LOC < 60s (measured). |
| **m1.06 — MCP v0** | 10 tools per `milestones/M1.md` epic m1.06 (canonical names in SOLO-09 §3). |
| **m1.07 — doctor & audit** | `engram doctor {status,egress,storage,embedder,config}` shipped (SOLO-13 §2.1's section milestone map names which sections land later); `audit.jsonl` written on every state-changing MCP call. |

**Exit gates.** The numeric gates are the rows in SOLO-13 §3
labelled `BUDGET (unmeasured)` with gate `M1`. M1 closes by
running the bench harness and either confirming each row or
filing an ADR for the miss. The non-numeric gates:

- All tests pass with `-race`.
- `golangci-lint` and `layercheck` clean.
- OQ-S001 resolved (see SOLO-OQ) — confirmed end-to-end at M1
  against the integrated `eng_search_semantic` path; M0 already
  validated the underlying vec0 budget in isolation.
- OQ-S006 re-verified on real-repo data; substantive miss against
  M0's curve files an ADR.

**Dependencies:** **M0 closed with a green or yellow outcome on
both spikes** (vector substrate and branch-in-PK schema). A red
outcome on either blocks M1 until the relevant ADR is amended or
the schema is rewritten.

**Vector substrate scope.** M1 ships brute-force `vec0` only.
The M1 product target is working sets at or below the
M0-measured vec0 ceiling (working number: ~250k embedded nodes;
SOLO-13 §3.3.1). HNSW is M2 work, not a contingent M1 fallback;
the pivot ADR is written at M2 entry against M1's measured
curves. PRODUCT.md "Vector substrate at M1" carries the user-
facing form.

---

## M2 — Identity, observability, plugin scaffolding

**Goal:** Plumb `actor_id` and `actor_kind` through every write;
land the single human-action gate; expand MCP to ~20 tools; declare the
plugin interfaces with no impls beyond defaults; turn on opt-in
OTLP and Prometheus.

| Epic | DoD |
|---|---|
| **m2.01 — actor on every write** | `actor_id` and `actor_kind` columns populated for every `nodes`, `edges`, `findings`, `suppressions`, `tasks`, `audit.jsonl` entry. |
| **m2.02 — human-action gate** | `eng_close_finding` for `severity=high` requires `actor_kind = 'human'`; refused otherwise with a clear error. |
| **m2.03 — MCP expansion** | Adds 9 tools (10 → 19 total) per `milestones/M2.md` epic m2.03; canonical names in SOLO-09 §3. |
| **m2.04 — plugin slots declared** | Go interfaces for `Tracker`, `VulnSource`, `Embedder`, `LLMGenerator`, `Notifier` in `core/ports/`; default impls in `infrastructure/`. No second impl. |
| **m2.05 — observability opt-in** | Prometheus `/metrics` (6 metrics, SOLO-13 §1.2), OTLP traces, both off by default; `engram doctor egress` reports listeners. |
| **m2.06 — HNSW substrate ADR + pivot** | OQ-S003 resolved: the vector-index port abstraction lands; lancedb-embedded vs. hnswlib(cgo) decision recorded; backup-tarball property preserved or regression documented; `engram doctor storage` `embeddings.substrate` flips to `"hnsw"` once the migration completes. Pivot is gated by M1's measured vec0 curves — green on M1 may legitimately defer the pivot to M3 if the ceiling proved generous. |

**Exit gates:**

- Every state-changing MCP call writes a complete `audit.jsonl` row.
- Human-action gate denial path covered by tests.
- `metrics.enabled = false` ⇒ no HTTP listener bound.
- Plugin interfaces stable; one impl per port.

**Dependencies:** M1.

---

## M3 — Pipelines and embedder

**Goal:** Promotion pipeline runs structural checks synchronously; an
async embedder worker keeps `node_embedding_refs` drained; vec0
search is the live path for `semantic_search`; auto-link
suggestions land; revalidation sweeps invalidate stale findings on
content drift.

| Epic | DoD |
|---|---|
| **m3.01 — promotion pipeline** | Structural checks (parse, dead-code, contract drift) run inside or immediately after the promotion transaction; findings emitted with `source_layer='structural'`. |
| **m3.02 — embedder worker** | Goroutine drains `node_embedding_refs` where `state='pending'`; throttled to a configurable rate; respects `engram_embed_queue_depth`. |
| **m3.03 — vec0 search live** | `semantic_search` queries `vec_nodes`; degraded fallback if model missing or vec0 unhealthy. |
| **m3.04 — auto-link** | post-promotion queue `work_kind='auto_link'` proposes `Edge` rows from embedding similarity above a threshold; surfaces as low-confidence findings until accepted. |
| **m3.05 — revalidation** | post-promotion queue `work_kind='revalidate'` sweeps open findings whose anchor content has changed; transitions to `closed` with `closed_reason='revalidated_obsolete'` (rule no longer fires) or `closed_reason='superseded_by_revalidation'` (rule fires on new content; new finding linked back). |

**Exit gates.** Numeric gates are SOLO-13 §3 rows gated `M3`
(embedder throughput, `semantic_search` recall, auto-link FP,
revalidation sweep). M3 closes by either confirming each row or
filing an ADR for the miss; OQ-S003 resolves at this milestone.
Non-numeric gates: auto-link surfaces as suggest-only (no
auto-merge) until calibrated.

**Dependencies:** M1, M2.

---

## M4 — Wiki mechanical kinds

**Goal:** Two mechanical wiki kinds (`hot_zone`, `entry_points`)
rendered to `docs/engram/`, plus the `eng_context_pack` MCP tool
for agents.

| Epic | DoD |
|---|---|
| **m4.01 — context pack** | `eng_context_pack` MCP tool returns a token-bounded bundle of nodes, recent commits, open findings, and tasks for a given symbol or task. |
| **m4.02 — hot_zone** | Mechanical page kind: top-N files by recent change frequency × blast radius. Rendered to `docs/engram/hot_zones.md`. |
| **m4.03 — entry_points** | Mechanical page kind: candidate "good first PR" symbols (low blast radius, tests adjacent, no open findings). Rendered to `docs/engram/entry_points.md`. |
| **m4.04 — wiki refresh** | `engram wiki` regenerates both kinds; runs on promotion via post-promotion queue `work_kind='wiki'`. |

**Exit gates.** Numeric gates are SOLO-13 §3.5 (`hot_zone`,
`entry_points`, `eng_get_context_pack`). Non-numeric gate: pages are
pure functions of promoted state with no LLM in the path.

**Dependencies:** M3.

---

## M5 — Optional review pipeline

**Goal:** Optional LLM-driven review (security, contract drift)
runs as a goroutine after promotion. Off by default. Honest cost story
in the docs. Findings surface via MCP; human-action gate applies.

| Epic | DoD |
|---|---|
| **m5.01 — review goroutine** | post-promotion queue `work_kind='review'`; `LLMGenerator` interface dispatches to local Ollama or configured remote endpoint. |
| **m5.02 — review prompts** | Versioned prompt set under `internal/application/review/prompts/`; each prompt addresses one finding kind. |
| **m5.03 — cost & quota** | Per-promotion token-budget cap; refusal with `degraded_reasons: ['review_quota_exceeded']` when over. Daily token total in `engram doctor`. |
| **m5.04 — surface findings** | Review findings carry `source_layer='semantic'`; visible via `eng_list_findings`; suppressible like any other. |

**Exit gates:**

- Review pipeline disabled by default; enabling requires explicit
  config opt-in and `engram doctor egress` reports the LLM target.
- Token-budget cap enforced; tested with a synthetic over-budget promotion.
- Documented dollar-cost example for one review of a 100-file commit
  against a hosted vendor LLM.

**Dependencies:** M3.

---

## M6 — Cutover

**Goal:** Promote `docs/docsv2solo/` to `docs/`; archive the prior
design tree.

| Epic | DoD |
|---|---|
| **m6.01 — promote design tree** | `docs/docsv2solo/design/` → `docs/design/`; old tree → `docs/archive/pre-solo/`. |
| **m6.02 — promote milestones** | `docs/docsv2solo/milestones/` → `docs/milestones/`; old M1–M8 → `docs/archive/pre-solo-milestones/`. |
| **m6.03 — promote operations** | `docs/docsv2solo/operations/` → `docs/operations/`; old files archived. |
| **m6.04 — fix references** | All cross-refs (`SOLO-NN`) audited and resolving; `CLAUDE.md` updated to point at new tree. |

**Exit gates:**

- No file under `docs/` references a `DV2-` ID.
- The pre-solo tree exists only under `docs/archive/`.
- `make features-index` (or its equivalent) regenerates without
  errors.

**Dependencies:** M5 in the field for at least one cycle (so the
docs match the running product).
