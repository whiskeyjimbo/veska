---
id: SOLO-00
title: "Veska Solo Design Set — Conventions"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
verified: true
verified_date: "2026-05-16"
---

# Solo Design Set

## Purpose

This set defines Veska's solo architecture: one daemon, one
developer, one machine.

## How to read this set

| If you are… | Read in this order |
|---|---|
| New to Veska | PRODUCT → SOLO-01 → SOLO-03 → SOLO-08 |
| An implementer | SOLO-04 → SOLO-08 → SOLO-07 → SOLO-11 → SOLO-09 |
| A reviewer | SOLO-01 → SOLO-15 → relevant section |

## Section index

| ID | Section | Owns |
|---|---|---|
| SOLO-00 | This file | Conventions |
| SOLO-01 | Charter | Identity, mission, non-goals, principles |
| SOLO-02 | Personas & Stories | Two personas (Dev, Agent), ~10 stories |
| SOLO-03 | The Daemon | Runtime topology, lifecycle, the developer journey |
| SOLO-04 | Domain Model | Entities, ports, invariants |
| SOLO-05 | Plugin Surface | Go interfaces with one impl each |
| SOLO-07 | Architecture | Layering rules, package layout, composition root |
| SOLO-08 | Data & Storage | SQLite schema, in-memory vector index, save/promote split |
| SOLO-09 | MCP Surface | ~30 tools, naming rules, output contract |
| SOLO-10 | Identity | One enum: `actor_kind` (`human \| agent \| system`). The whole story |
| SOLO-11 | Pipelines | Save (in-memory) vs. promotion (SQLite + post-promotion queue) |
| SOLO-12 | Wiki | `eng_context_pack` + 2 mechanical page kinds |
| SOLO-13 | NFR | Observability, perf budgets, degraded modes |
| SOLO-14 | Roadmap | Milestones M1–M5; cutover at M6 |
| SOLO-15 | Glossary | Authoritative ubiquitous language |
| SOLO-16 | Error catalogue | Every `veska_code`, exit code, audit shape |
| SOLO-17 | Lifecycle & Operations | Schema migrations, upgrade-during-run, restore, install/uninstall |
| ADR-* | ADRs | Discrete decisions cited from narrative docs |

**SOLO-06 numbering gap.** SOLO-06 is intentionally unused; the
prior design tree assigned `06` to a section that was retracted
in the solo redesign. Numbers are not reused across the gap.

## Conventions

### Stable IDs

| Artifact | Format | Example |
|---|---|---|
| Section | `SOLO-NN` | `SOLO-08` |
| Sub-file | `SOLO-NN.MM` | `SOLO-08.02` |
| User story | `US-NN.MM` | `US-04.01` |
| ADR | `ADR-NNNN` | `ADR-S0001` (S prefix = solo redesign) |

Cross-references use IDs, not paths.

### Frontmatter

```yaml
---
id: SOLO-NN[.MM]
title: "..."
status: draft|in-review|approved
version: 0.1.0
last_reviewed: YYYY-MM-DD
related: [SOLO-..., ADR-S0..., US-...]
---
```

### Style

- US English, ISO 8601 dates, SI units.
- Tables for enumerations; prose for rationale.
- Use plain names for what's actually there: "channel", "queue",
  "table", "Go interface". Avoid pattern-language jargon
  (saga, post-promotion queue coordinator, bounded context) unless the pattern
  is literally what's being implemented.
- No emoji unless representing a literal symbol.
- No internal-process vocabulary in normative text. ADRs cite
  decisions; reviews live in commit history.

### What we don't write

- **Capability schemas as build artifacts.** Plugin interfaces are
  Go interfaces. A second impl gets an ADR.
- **Per-doc changelogs.** Git is the changelog.
- **"Future drop-in" promises.** If a port might move
  out-of-process later, that's a future ADR. Not a present design
  constraint.
- **Disclaimers about deferred features.** Deferred features live
  under `deferred/`. They are not in this doc tree.

## Definition of Done (per doc)

A doc transitions to `approved` only when:

1. Frontmatter is valid.
2. All cross-references resolve.
3. All decisions are inline-trivial or backed by an ADR.
4. Every performance number lives in SOLO-13 §3 under one of the
   labels `BUDGET (unmeasured)`, `BUDGET (measured M<N>)`,
   `INVARIANT`, or `DEFAULT`. Other docs cross-reference SOLO-13
   instead of repeating numbers.
5. At least one reviewer outside the author has signed off.

## Repository layout

This set lives at `solov2/docs/design/`. It is the active design
tree; there is no separate prior set preserved alongside it.
