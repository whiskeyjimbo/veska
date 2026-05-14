---
id: ADR-S0013
title: Branch-PK delta scheme — fallback storage layout if OQ-S006 lands red
status: proposed
date: 2026-05-09
deciders: [whiskeyjimbo]
related: [ADR-S0001, SOLO-08]
---

# ADR-S0013 — Branch-PK delta scheme (fallback)

## Status

**Proposed (draft).** This ADR is pre-authored insurance. It does not
ship at M1 unless OQ-S006 (the M0 branch-PK row-growth spike,
re-verified at M1 close) lands red. If the spike lands green, this
ADR moves to *withdrawn* and the SOLO-08 §4 design stands as-is.

The point of pre-authoring it is to avoid having to invent the
escape hatch under M1 deadline pressure. The principal-architect
review of 2026-05-09 named this as the largest unspecified failure
mode in the design.

## Context

SOLO-08 §4 makes `branch` part of the composite primary key on
`nodes`, `edges`, `findings`, and `cross_repo_edge_stubs`. The
storage cost is `O(symbols_touched × branches)` for the four
tables — embeddings dedupe via content addressing, but the row
tables do not.

OQ-S006 measures the actual cost on synthetic and real-repo
workloads. The risk is concrete: a developer with 50 active
branches that share a 100k-symbol core stores 5M `nodes` rows
plus matching `edges`/`findings`. At that population SQLite is
still fine, but the daemon's RSS curve, branch-GC sweep cost, and
the `repo add` admission projection all degrade noticeably.

If the spike is red, the design needs a storage shape that keeps
correctness (per-branch state for findings, per-branch content
hashes for nodes) without storing a row per `(symbol, branch)`
when the symbol is identical across branches.

## Decision (conditional)

**If OQ-S006 lands red, switch the four affected tables to a
"trunk + delta" scheme.** Each table splits into two physical
tables sharing the same logical view:

```sql
-- The "trunk" — one row per (symbol, content_hash). Branch-agnostic.
-- The trunk row is shared across every branch whose content matches.
CREATE TABLE nodes_trunk (
    node_id        TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    content_hash   TEXT NOT NULL,
    language       TEXT NOT NULL,
    kind           TEXT NOT NULL,
    symbol_path    TEXT NOT NULL,
    file_path      TEXT NOT NULL,
    line_start     INTEGER,
    line_end       INTEGER,
    first_seen_at  INTEGER NOT NULL,
    PRIMARY KEY (node_id, content_hash),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);

-- The "delta" — one row per (node, branch). Records which content_hash
-- the branch points at, plus per-branch metadata that *must* be
-- per-branch (actor attribution at the per-branch promotion).
CREATE TABLE nodes_branch (
    node_id          TEXT NOT NULL,
    branch           TEXT NOT NULL,
    repo_id          TEXT NOT NULL,
    content_hash     TEXT NOT NULL,             -- points at nodes_trunk
    last_promoted_at INTEGER NOT NULL,
    actor_id         TEXT NOT NULL,
    actor_kind       TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system')),
    PRIMARY KEY (node_id, branch),
    FOREIGN KEY (node_id, content_hash) REFERENCES nodes_trunk(node_id, content_hash),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);

CREATE INDEX idx_nodes_branch_active ON nodes_branch(repo_id, branch);
CREATE INDEX idx_nodes_trunk_symbol ON nodes_trunk(symbol_path);
```

Logical reads use a view:

```sql
CREATE VIEW nodes AS
SELECT
    nb.node_id,
    nb.branch,
    nb.repo_id,
    nt.language,
    nt.kind,
    nt.symbol_path,
    nt.file_path,
    nt.line_start,
    nt.line_end,
    nt.content_hash,
    nb.last_promoted_at,
    nb.actor_id,
    nb.actor_kind
FROM nodes_branch nb
JOIN nodes_trunk nt USING (node_id, content_hash);
```

`edges`, `findings`, and `cross_repo_edge_stubs` get the same
treatment. The `Graph` aggregate's read methods (SOLO-04 §5.3)
read from the view; write methods write trunk + delta in one
transaction.

### Storage shape

Symbols stable across N branches: 1 trunk row + N delta rows
(typically much smaller than the trunk). Symbols that diverge:
M trunk rows (one per content_hash) + N delta rows (each pointing
at the right trunk).

The savings depend on cross-branch content overlap. The OQ-S006
spike is what answers "is the overlap high enough to bother."
Synthetic estimates: a 50-branch repo with 80% per-branch overlap
of a 100k-symbol core saves ~70% of `nodes` rows compared to the
flat composite-PK scheme.

### Migration path

If the spike lands red and we trip this ADR:

1. M1's m1.03 schema work targets trunk+delta from the first
   migration, not flat composite PK. There is no "migrate live
   data from flat → trunk+delta" because no V2 daemon has shipped
   yet. M1 just doesn't ship the flat schema.
2. The MCP read tools (`eng_get_node`, `eng_find_symbol`, etc.)
   read the view; they need no surface change.
3. The promotion pipeline writes trunk + delta in the same
   `BEGIN IMMEDIATE`. Trunk row is upserted (`INSERT ... ON
   CONFLICT DO NOTHING`); delta row is unconditionally inserted.
4. Branch GC (`veska gc --branches`) removes delta rows whose
   `branch` is gone, then sweeps trunk rows with no remaining
   delta references.
5. `veska doctor storage` reports trunk-vs-delta row counts so
   the operator can see overlap.

### What stays the same

- The `Node` / `Edge` / `Finding` aggregates (SOLO-04) are
  unchanged. The aggregate fields are unchanged. The trunk/delta
  split is a storage detail behind `GraphRepository`.
- The save/promote split is unchanged. Staging stays per-file.
- The cross-repo resolver chain (SOLO-11 §9) reads the view, so
  its query shape is unchanged.
- `node_embeddings` is already content-addressed; this ADR does
  not change embedding storage.

## Consequences

**Pros:**

- Bounded row count for stable symbols across many branches.
- Trunk content_hash already exists in the design (SOLO-08 §3.1
  `nodes.content_hash`); this scheme just makes it a key.
- The view keeps every read path source-compatible.

**Cons:**

- Two tables per logical entity. Migration files become longer.
- Writes are two inserts in one transaction (small constant cost
  per promoted row).
- Branch GC has a two-step sweep (delta first, then trunk).
- Schema readability suffers; the view is the read surface but
  developers debugging will still poke at trunk/delta directly.
- `cross_repo_edge_stubs` complicates because stubs key on
  source-side `(node_id, branch)` plus resolver-side identifiers.
  Stub trunk vs. stub delta is not as natural a split — likely
  stubs stay flat-PK as a small exception.

## Alternatives considered

1. **Per-branch shadow tables** (`nodes_branch_<sanitized>`).
   Rejected: SQLite handles many tables fine but `veska gc
   --branches` becomes a DDL operation.
2. **Branch-as-bitmap.** Store `branches BLOB` on each node,
   bit-set per branch. Rejected: branch IDs are strings (refs),
   not small integers; the mapping table re-introduces the
   problem.
3. **Drop branch from PK, single-branch-only V2.0.** Rejected:
   solo developers routinely have multiple feature branches in
   flight; "we only index your active branch" defeats the
   blast-radius and `query_as_of` use cases.

## Open questions

- **OQ-S013-1**: Does the JOIN cost in the view dominate read
  latency at 5M trunk rows? The §3.1 `find_symbol` budget would
  measure this on the spike fixture.
- **OQ-S013-2**: Does `cross_repo_edge_stubs` need the same
  split, or does flat-PK suffice given typical stub volume?
  Spike measures.

These are bench items for the trip-condition: if OQ-S006 lands
red and this ADR moves to *accepted*, the M1 substrate epic
(m1.03) absorbs the OQ-S013-* benches as part of its exit gate.

## Decision authority

If OQ-S006 lands red, this ADR moves from *proposed* to
*accepted* without further design work. The trip is mechanical:
the M0 close report cites the measured per-branch row growth,
compares to `[branch_pk].max_growth_per_branch_pct` (DEFAULT 10%
of trunk row count per active branch; CONFIG-SURFACE), and
either confirms the flat scheme stays or trips this ADR.

If OQ-S006 lands green, this ADR is retired (status: withdrawn)
and SOLO-08 §4 stands.
