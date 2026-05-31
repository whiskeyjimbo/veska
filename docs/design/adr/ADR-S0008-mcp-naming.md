---
id: ADR-S0008
title: MCP tool naming — eng_<verb>_<object>, closed verb set
status: accepted
date: 2026-05-08
deciders: [whiskeyjimbo]
verified: true
verified_date: "2026-05-16"
---

# ADR-S0008 — MCP tool naming — `eng_<verb>_<object>`, closed verb set

## Context

V1 shipped 25 MCP tools with mixed naming (`find_symbol`,
`get_call_chain`, `task_set_active`, `find_todos`). The prefixes
were inconsistent (`task_*`, `find_*`, `get_*`, bare verbs), tools
collided with other MCP servers' names, and several verbs (`find`
vs. `get` vs. `query` vs. `view`) overlapped semantically.

The prior V2 design prefixed everything with `eng_` (ADR-0022) and
then proposed a 60+ tool surface. Solo cuts to ~30 and uses this
ADR to lock the naming pattern before the surface grows again.

Agents are the primary consumer of MCP tool names. Stable, terse,
predictable names beat creative ones.

## Decision

Every Veska-provided MCP tool is named:

```
eng_<verb>_<object>[_<qualifier>]
```

The `eng_` prefix avoids collisions with other MCP servers in the
host. It is mandatory.

**The verb set is closed at V2.0**:

| Verb | Use for |
|---|---|
| `find` | Lookup by partial info; returns 0..N matches, unranked. |
| `get` | Lookup by ID; returns 0..1 result, errors if unknown. |
| `list` | Enumerate within a small bounded scope (a file, a package, a task). |
| `search` | Ranked search — vector or full-text. |
| `set` | Pin/select a single-cardinality value (e.g. the active task). |
| `close` | Transition a finding / suppression / task to terminal state. |
| `reopen` | Reverse a `close`. Inverse pair. |
| `suppress` | Apply a suppression to a finding. |
| `add` | Bring an external thing under the daemon's management (a repo). |
| `remove` | Inverse of `add`. |

This set is the union of every verb in use across the V2.0 MCP
inventory (SOLO-09 §3). New verbs require an ADR amendment to this
file. SOLO-09 cites this list; it does not maintain its own.

Verbs intentionally **not** in the set:

- `walk` — graph traversal is shaped as `get` (returns one chain / radius).
- `open` — covered by `reopen`; a finding's first transition is implicit at creation.
- `index`, `promotion` — CLI commands, not MCP tools (the *"diagnostics that fix things are CLI-only"* rule in SOLO-09 §3.7).
- `inspect` — same rule.
- `create`, `update`, `link` — no V2.0 caller. Add via ADR amendment when a tool actually needs them (likely when tracker-write or analysis-slot tools land in M2/M3).

The object set is open but drawn from the glossary nouns
(`symbol`, `node`, `edge`, `file`, `task`, `finding`, `suppression`,
`call_chain`, `blast_radius`, `context_pack`, `wiki_page`, etc.).

A qualifier is allowed but optional; e.g.,
`eng_find_symbol_by_path`. Qualifiers are required only when two
tools would otherwise collide.

**Aliases on rename.** When a tool is renamed (verb changes, object
changes, or qualifier added), the old name remains a working alias
for **one minor release** with a `deprecated: <new-name>` field on
its description. After one minor release, the alias is removed.

The audit log records the alias name actually called, not the
canonical name, so post-hoc analysis can spot agents still on the
old name.

The total tool count at V2.0 target is **~30** (down from V1's 25
+ the prior design's 60+). Adding a tool is an entry in SOLO-09;
removing or renaming one is an alias-and-deprecate cycle.

## Consequences

Positive:

- Agents trained on the verb set generalise; they guess
  `eng_find_finding` correctly without docs.
- Renames have a defined deprecation path, not a flag-day.
- The 10-verb closed set forces deliberate choice when adding a
  tool; "should this be `find` or `search`?" becomes a real
  question rather than a coin flip.

Negative:

- A new use case that does not fit the verb set requires either an
  unsatisfying mapping or an ADR to extend the set. The first
  occurrence of either is information.
- Aliases double the surface for one release window. Acceptable.

## Alternatives Considered

- **Open verb set, prefix only.** What V1 did. The verb sprawl was
  the actual UX problem; locking the prefix without locking the
  verbs solved nothing.
- **No aliases; flag-day renames.** Cheaper to maintain, but breaks
  agents pinned to a specific tool name in their prompt or skill.
  One minor release of overlap is cheap insurance.
- **Permanent aliases, never removed.** Surface area grows
  unboundedly. We pay the one-release cost to avoid that.

## References

- SOLO-09 (MCP surface — full tool inventory)
- Prior ADR-0022 (the `eng_` prefix; this ADR carries the prefix
  forward and adds the verb set)
