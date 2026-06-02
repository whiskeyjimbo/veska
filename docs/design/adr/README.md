# Architecture Decision Records — Veska Solo

This directory holds ADRs cited from the SOLO design set. ADRs are
MADR-style; each one records a discrete decision worth pinning.

The solo redesign retracts eight prior V2 ADRs fully plus one
partially (ADR-0019) — nine retraction entries in total. See
[`RETRACTED.md`](RETRACTED.md) for the list and rationale. The
remaining prior ADRs that are still relevant carry forward
unchanged and are referenced by their original numbers.

## Format

```
---
id: ADR-S00NN
title: <Title>
status: accepted
date: YYYY-MM-DD
deciders: [whiskeyjimbo]
---

# ADR-S00NN — <Title>

## Context
## Decision
## Consequences
## Alternatives Considered
```

The `S` prefix denotes the solo redesign. Numbers are sequential
and never reused.

## Index

| ID | Title | Status |
|---|---|---|
| [ADR-S0001](ADR-S0001-sqlite-substrate.md) | SQLite + sqlite-vec as the V2.0 substrate (staged adoption) | amended (vector portion superseded by ADR-S0014/S0015) |
| [ADR-S0002](ADR-S0002-hexagonal.md) | Hexagonal layering, one impl per port at V2.0 | accepted |
| [ADR-S0003](ADR-S0003-save-vs-promote.md) | Save-vs-promote split with volatile staging | accepted |
| [ADR-S0004](ADR-S0004-post-promotion-queue-table.md) | One post_promotion_queue table, four work-kinds, one goroutine each | accepted |
| [ADR-S0005](ADR-S0005-actor-id-and-actor-kind.md) | actor_id + actor_kind is the entire identity model | accepted |
| [ADR-S0006](ADR-S0006-five-edge-kinds.md) | V2.0 ships five structural EdgeKinds (amended — shipped enum has six, adding non-structural SIMILAR_TO) | amended |
| [ADR-S0007](ADR-S0007-embedder-swap.md) | Embedder swap is one CLI subcommand against a live daemon | accepted |
| [ADR-S0008](ADR-S0008-mcp-naming.md) | MCP tool naming — `eng_<verb>_<object>`, closed verb set | accepted |
| [ADR-S0009](ADR-S0009-server-tier-out-of-scope.md) | Server tier is out of scope for the V2.0 doc tree | accepted |
| [ADR-S0010](ADR-S0010-no-lint-bonanza.md) | One custom lint analyser at V2.0 — `layercheck` | accepted |
| [ADR-S0011](ADR-S0011-writer-pool-model.md) | Two-pool single-writer model via `database/sql` | accepted |
| [ADR-S0012](ADR-S0012-rename-seal-to-promote.md) | Rename "Seal" to "Promote/Promotion" across the design tree | accepted |
| [ADR-S0013](ADR-S0013-branch-pk-delta-scheme.md) | Branch-PK delta scheme — fallback storage layout if OQ-S006 lands red | proposed |
| [ADR-S0014](ADR-S0014-hnsw-pivot.md) | HNSW vector index pivot (replaces vec0 above 100k nodes) | accepted (extended by ADR-S0015) |
| [ADR-S0015](ADR-S0015-vec-pivot-dual-backend.md) | Dual-backend vector strategy (sqlite-vec default, usearch above threshold) | accepted |
| [ADR-S0016](ADR-S0016-memvec-rename.md) | Rename the in-memory vector backend (sqlitevec → memvec) | accepted |
| [ADR-S0017](ADR-S0017-portable-node-identity.md) | Portable content-derived node identity for a shared multi-contributor graph | proposed |
| [ADR-S0018](ADR-S0018-shared-graph-data-partitioning.md) | Shared-graph data partitioning — shared truth vs. local state | proposed |

## Retracted from prior set

These prior ADRs are explicitly retracted by the solo redesign.
See [`RETRACTED.md`](RETRACTED.md) for details.

| Prior ID | Retraction | Title | Replaced by |
|---|---|---|---|
| ADR-0008 | full | Transport & Auth for Networked Modes | (none — Unix socket only) |
| ADR-0009 | full | Executor Slot Cardinality | (none — no executor slot) |
| ADR-0011 | full | `finding-revalidator` slot | (none — revalidation is a goroutine; temporal optimization folded into SOLO-11 §6) |
| ADR-0012 | full | `AuditRun` identity and resume | (none — `audit.jsonl` has no aggregate) |
| ADR-0014 | full | Typed plugin registry | ADR-S0002 (plain Go interfaces) |
| ADR-0015 | full | Embedder migration ceremony | ADR-S0007 |
| ADR-0018 | full | Eager attribution | ADR-S0005 (`actor_id` + `actor_kind`) |
| ADR-0023 | full | Branch-per-Git-branch policy | ADR-S0001 (`branch` is a column) |
| ADR-0019 | partial | Embedding queue lives in Dolt | ADR-S0004 (queue in SQLite `post_promotion_queue`); durable-queue decision carries forward |

Eight ADRs are fully retracted; ADR-0019 is partially retracted (its
durable-queue decision carries forward). Nine retraction entries total.

## Carried forward from prior set

These prior ADRs remain relevant in solo and are referenced
unchanged. They have no `S`-prefix counterpart because the
decision is unchanged:

- ADR-0001 — Local-first dev loop is mode-invariant.
- ADR-0005 — Dead-code detection rules.
- ADR-0013 — Signal layers as a `source_layer` tag.
- ADR-0016 — Auto-link confidence formula.
- ADR-0020 — Suppression conflict resolution.
- ADR-0021 — Symbol anchor stability.
- ADR-0022 — MCP `eng_` prefix (extended by ADR-S0008).

When in doubt, the SOLO design set narrative cites the ADR
authoritative for that decision. Solo ADRs win where they conflict.
