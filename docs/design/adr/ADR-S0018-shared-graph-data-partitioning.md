---
id: ADR-S0018
title: "Shared-graph data partitioning — shared truth vs. local state"
status: Proposed
date: 2026-06-02
deciders: [whiskeyjimbo]
supersedes: []
extends: []
related: [ADR-S0017, ADR-S0009, ADR-S0005, ADR-0020]
verified: false
---

# ADR-S0018 — Shared-graph data partitioning: shared truth vs. local state

The foundational design for sharing one veska graph across a few contributors:
**which rows/columns are shared truth, which are per-contributor local, and how
they are physically separated** — _before_ any sync/merge transport is built.
Transport and conflict-resolution mechanics are explicitly deferred (see
_Out of scope_).

## Status

Proposed. This is the opening design section of the (not-yet-scoped) shared-DB
epic. It depends on [[ADR-S0017]] (portable node identity) as a precondition:
S0017 makes the graph *converge* across checkouts; this ADR decides what is
worth sharing once it does.

## Context

veska's graph lives in a single SQLite database (`go-sqlite3`; the migrations in
`internal/infrastructure/sqlite/migrations/`). It was designed local-only —
[[ADR-S0009]] explicitly put a server/multi-tenant tier out of scope — so the
schema freely **mixes shared truth with per-contributor local state in the same
tables**. Examples already visible today:

- `repos.root_path` is one contributor's checkout path; `repos.active_branch` is
  whatever branch *they* have out.
- `post_promotion_queue` is one daemon's work backlog.
- `tasks` enforces `UNIQUE(repo_id) WHERE active = 1` — **one active task per
  repo globally**, which snaps the moment two people work the same repo.
- `repo_aliases.name` is a globally-unique PK — a personal typing shortcut
  promoted to a contended global resource.

Once [[ADR-S0017]] makes `node_id` converge across checkouts, *sharing becomes
possible* — but only if we first separate the shared truth from the local junk.
A classification is therefore the prerequisite for any transport. This also
re-tensions [[ADR-S0009]]: "share a SQLite DB with a few people" has no built-in
answer, and reopening it deliberately is part of this work.

## Decision

### 1. Three data classes

| Class | Definition | Convergence | Examples |
|-------|------------|-------------|----------|
| **A — shared derived truth** | A pure function of (source @ branch+sha, identity scheme, **extraction-tool version**, embedding model) | Converges across contributors (S0017) | `nodes`, `edges`, `cross_repo_edge_stubs`, `file_imports`, `node_fts_*`, `node_embeddings` |
| **B — shared human curation** | Human decisions a team wants shared; does NOT auto-converge; needs attribution + merge | Conflicts possible | `suppressions`, finding **triage state**, (optionally) the task **backlog** |
| **C — per-contributor local** | Meaningless or harmful to share | N/A | `repos` registration/working columns, `post_promotion_queue`, `node_embedding_refs.state`, `daemon_state`, `tasks.active`, `repo_aliases` |

### 2. The splits are often **intra-table (column-level)**, not whole-table

Three tables straddle the line and must be split by column, not assigned wholesale:

- **`repos`** → shared **identity core** (`repo_id`, `module_path`,
  `canonical_url`) vs. local **registration/working state** (`root_path`,
  `added_at`, `active_branch`, `last_promoted_sha`, `kind`, `last_accessed_at`,
  `prompted_at`).
- **`findings`** → shared **derived existence** (`finding_id`, anchor, `rule`,
  `severity`, `message` — re-derivable, class A) vs. shared **triage state**
  (`state`, `closed_reason`, `closed_at`, closing actor — class B curation).
- **`tasks`** → shared **backlog** (the work item) vs. local **activation**
  (`active`; the `UNIQUE(repo_id) WHERE active=1` constraint is the concrete
  thing that breaks under sharing).
- **`node_embedding_refs`** → shared **`node_id → content_hash` link** (class A,
  re-derivable) vs. local **embed state machine** (`state`, `attempts`,
  `enqueued_at` — class C, each daemon's own embedder progress).

### 3. Version-homogeneity is the precondition for sharing class A

Class A converges only when contributors agree on the **producing tool
versions**, not just identity + model. Two axes:

- **Embedding model.** Identical content under a different model yields a
  different vector. (Latent bug today: `node_embeddings` PK is `content_hash`
  *alone* — a shared DB spanning two models would collide one row, and the
  `node_embedding_refs.content_hash` FK breaks with it.)
- **Extraction/parser version.** Alice on an older Go parser and Bob on an
  improved one produce *different* `nodes`/`edges` from byte-identical source,
  even with S0017's converged IDs.

Rather than push `(content_hash, model)` and version columns through every
table, fold both into **one principle**:

> A shared graph store is **version-homogeneous**: it pins exactly one
> extraction-tool version and one embedding model, recorded in its own
> `database_meta`. A daemon attaching to a shared store with a mismatched parser
> version or embedding model is **refused (or mounts read-only)** — the same
> refuse-to-start posture the vector backend already uses
> (`internal/infrastructure/vector/CLAUDE.md`) and the same shape as S0017's
> tier-mixing `doctor` warning.

This turns "is this derived row safe to share?" from a per-row concern into a
single boundary check at attach time. The model-PK collision is **evidence for**
this principle, not a key-redesign task.

### 4. Share vs. regenerate — the value is concentrated

Class A subdivides by **reproducibility**, which changes *what actually needs to
travel in the shared store*:

| Sub-class | Tables | Cost to rebuild locally |
|-----------|--------|------------------------|
| Cheap to regenerate | `nodes`, `edges`, `stubs`, `file_imports`, `node_fts_*` | re-parse shared source — fast, no external dep |
| Expensive | `node_embeddings` | needs the model + Ollama; the real cost |
| Irreproducible | class B (`suppressions`, triage) | human input — cannot be rebuilt |

So the strongest posture is **share what can't be cheaply rebuilt, regenerate
the rest**: the shared store carries **embeddings + class B**, and each
contributor *locally regenerates* the cheap derived graph from shared source.
This **sidesteps parser-version parity** for the cheap tables entirely (each
contributor's local parser produces their own copy) while still reaping the
expensive dedup (nobody re-embeds what a teammate already embedded).

[[ADR-S0017]] survives — indeed is *required* — under this posture: converged
`node_id` is the **join key** between shared embeddings/findings and the
locally-regenerated nodes. Without it the join is per-checkout garbage.

(A convenience variant — also ship the cheap derived tables so a fresh
contributor has a zero-rebuild graph — is allowed but then re-imposes
version-homogeneity on those tables. The decision is per-deployment; the
classification above is what makes it expressible.)

### 5. Mechanism — two physical stores, not actor-scoped rows

Separate class C from A/B by **storage**, not by adding `actor_id` columns:

- A **shared graph store** (the thing that gets shared) holds class A (per the
  share/regenerate decision) + class B.
- A **local state store** (never shared) holds class C.
- **Each store carries its own `database_meta`** — schema version + migration
  ledger — since they version independently.

Rejected: keeping one DB and actor-scoping the local tables. Every
contributor's transient junk (queue rows, embed attempts, LRU timestamps) would
ride inside the shared artifact, churning it on every sync and inviting
conflicts on data that has no business being shared.

**Aliases** (the question that surfaced this ADR) resolve cleanly under this:
they are personal naming → **class C, local store**. (If a shared team
vocabulary is later wanted, it is an opt-in class-B table keyed by actor, not
the global-unique-name table that exists today.)

## Out of scope (deferred to follow-on design)

- **The sharing transport itself.** Named candidates, not chosen here: **Dolt**
  (git-for-SQL, *already* in-stack for beads via `.beads/`, so the operational
  pattern exists) for a versioned/mergeable shared store; or periodic
  copy/sync of a read-mostly shared SQLite file. The two-store split is designed
  to be transport-agnostic, but the transport choice will pin details (e.g.
  whether class B uses Dolt's merge or an app-level CRDT).
- **Class-B conflict resolution.** Two contributors triaging the same finding,
  or suppressing the same target, need a merge policy — relates to and may
  extend [[ADR-0020]] (suppression conflict resolution).
- **Reopening [[ADR-S0009]].** Any networked/canonical-store shape is a
  deliberate reversal of the local-only scope and gets its own ADR.

## Open questions

- **Branch axis.** `nodes`/`edges`/`findings` are branch-keyed. Presumably only
  *shared* branches (e.g. `main`) belong in the shared store; a contributor's
  personal feature branch stays local. This couples to `last_promoted_sha` being
  class C and needs an explicit rule for "which branches are shareable."
- **Task ownership.** Is the backlog genuinely shared (team tracker) or is the
  whole `tasks` table local? Depends on whether veska tasks are a team artifact
  or a personal working set.
- **Finding-triage sharing.** Is a closed/suppressed finding a team decision
  (shared, class B) or personal? Default assumption: team-shared, attributed.

## Consequences

- A clear, testable rule for every table/column on the shared-vs-local axis —
  the foundation any transport must respect.
- Two stores to manage (and to surface in `doctor`); `database_meta` lives in
  both.
- The shared store is **version-homogeneous**, enforced at attach — a new
  operational invariant.
- `node_embeddings` PK gains an effective model dimension (via the
  version-homogeneity boundary, not necessarily a column) — the latent
  multi-model collision is closed.
- [[ADR-S0017]] is confirmed as a hard precondition: converged `node_id` is the
  join key that makes share-vs-regenerate work.
- No transport is committed; the classification stands independently of how
  sharing is ultimately wired.
