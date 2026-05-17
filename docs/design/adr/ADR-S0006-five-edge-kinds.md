---
id: ADR-S0006
title: V2.0 ships five EdgeKinds
status: accepted
date: 2026-05-08
deciders: [whiskeyjimbo]
verified: true
verified_date: "2026-05-16"
---

# ADR-S0006 — V2.0 ships five EdgeKinds

> **Factual-divergence note (2026-05-16).** The shipped code in
> `internal/core/domain/edge.go` defines **six** `EdgeKind`
> values: the five structural kinds decided here plus
> `SIMILAR_TO`, added in M3 for the auto-link pipeline. `SIMILAR_TO`
> is a proposed semantic-similarity edge written with
> `Confidence=Unresolved` and paired with a `source_layer='semantic'`
> Finding — it is not a tree-sitter-parsed structural edge, so it
> does not change the parser-cost reasoning below. This ADR records
> the structural-edge decision and is not rewritten; the
> `SIMILAR_TO` addition needs its own amending ADR. Until then, treat
> the "five" count here as the *structural* set, not the total
> `EdgeKind` enum size.

## Context

The prior V2 design ratified an additive set of fifteen EdgeKinds
(seven structural, ten data-flow) including `READS`, `WRITES`,
`MUTATES`, `THROWS`, `CATCHES`, `OVERRIDES`, `DECORATES`,
`INSTANTIATES`, etc. The justification was downstream features —
reachability-aware vuln scoring, schema graph, decorator-driven
frameworks — that all sit at M5/M6 in the prior roadmap.

Solo's M1 needs nodes/edges good enough to answer:

- "Where is `Foo`?" — `find_symbol`.
- "Who calls `Bar`?" — `get_call_chain` over `CALLS`.
- "What's in this file?" — `CONTAINS`.
- "What tests cover `Baz`?" — `TESTS`.
- "What does this package depend on?" — `DEPENDS_ON`.
- "What does this module import?" — `IMPORTS`.

That is six. We can fold "what's in this file/package" and
"what does this module import" using the structural edges below.

Every EdgeKind we ship costs us a per-language tree-sitter pass,
test fixtures across Go/TypeScript/Python, and a row in the resolver
arity matrix. Shipping the data-flow set on day one means we do six
languages × ten edges of authoring before M1 exit.

## Decision

V2.0 ships **five EdgeKinds**:

| Kind | Semantic | Allowed `(src, tgt)` |
|---|---|---|
| `CALLS` | Subject calls target | `(function|method|test) → (function|method)` |
| `IMPORTS` | Subject imports target symbol/module | `(file|package) → (file|package|function|type)` |
| `CONTAINS` | Subject contains target lexically | `(package → file), (file → function|type|method), (type → method|field)` |
| `TESTS` | Test exercises target | `test → (function|method|type)` |
| `DEPENDS_ON` | Manifest-level dependency | `package → package` |

Resolver enforces arities; an adapter that emits a kind with a
disallowed `(src.kind, tgt.kind)` pair fails the promotion.

NodeKinds at V2.0 stay coarse: `function`, `method`, `type`, `file`,
`package`, `test`, `field` (kept from the prior design because
`CONTAINS type → field` is cheap and useful).

**Deferred to a future ADR:**

- `READS`, `WRITES`, `MUTATES` — wait until the schema-graph epic
  has a real consumer.
- `THROWS`, `CATCHES` — wait until error-path blast radius has a
  user asking for it.
- `OVERRIDES`, `DECORATES`, `INSTANTIATES` — wait until a per-language
  parser pass earns them.

The deferral is not a "we'll add this in M2" promise. The
EdgeKinds set grows when a feature needs it and a measurement shows
the parser cost is bounded. Until then, five.

**OO-language follow-up.** OQ-S008 carries the open question of
whether `IMPLEMENTS` and `EXTENDS` are required when Java,
Python, or C# parsers ship. The five-edge claim above is for
Go + TypeScript + JavaScript and their `.tsx`/`.jsx` siblings;
adding an OO language without IMPLEMENTS/EXTENDS may be the
wrong call, and the resolution is "measure against real
queries, then ADR." No OO parser ships in V2.0, so the question
parks until OQ-S008 has a real workload to test against.

## Consequences

Positive:

- M1 ships **five tree-sitter grammars × five edges** (Go, TS,
  TSX, JS, JSX — five grammars, two `symbol_path` namespaces;
  see SOLO-04 §5.1.1). The earlier "six languages" framing
  conflated grammars and namespaces; the corrected count is
  five grammars over two address spaces. Cuts parser-authoring
  scope by a third without losing any of the V1-parity MCP
  tools.
- Smaller resolver matrix; the arity table fits on one screen.
- Edge volume on a 100k-LOC repo stays well inside SQLite's
  comfort zone, simplifying SOLO-13 perf budgets.

Negative:

- `task_blast_radius` and friends walk `CALLS` only at V2.0; the
  "what mutated" axis the prior design promised is deferred.
- Reachability-aware vuln scoring is not in V2.0. The vuln source
  surfaces findings; reachability filtering is a future ADR.
- Frameworks that bind by decorator (Flask routes, React components
  via `@component`) lose the decorator-as-anchor pattern. They show
  up as plain functions until `DECORATES` is added.

We accept the cuts. If a user asks for one of the deferred kinds,
that is an ADR with a justification grounded in usage, not a
roadmap entry.

## Alternatives Considered

- **Ship the full fifteen-kind set.** Right answer for the prior
  server-tier roadmap; wrong for solo M1. The cost is concrete
  (parser passes, test fixtures, arity matrix) and the benefit is
  unresolved.
- **Ship three (`CALLS`, `IMPORTS`, `CONTAINS`).** Loses `TESTS`
  and `DEPENDS_ON`, which the V1 tool surface relies on. Too thin.
- **Ship a single generic `RELATES` kind with a `details.subkind`
  string.** Loses arity enforcement; agents would need to filter
  on a string column. The strong-enum approach is better for
  agents and for lint.

## References

- SOLO-04 (domain model — EdgeKind enum)
- SOLO-15 (glossary — Edge)
- Prior ADR-0006 (the fifteen-kind set; superseded for V2.0)
