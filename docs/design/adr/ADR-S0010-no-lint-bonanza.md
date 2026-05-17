---
id: ADR-S0010
title: One custom lint analyser at V2.0 — layercheck
status: accepted
date: 2026-05-08
deciders: [whiskeyjimbo]
verified: true
verified_date: "2026-05-16"
---

# ADR-S0010 — One custom lint analyser at V2.0 — `layercheck`

## Context

The prior V2 design specified fourteen custom Go analysers under
`tools/lint/`:

- `layercheck` — domain imports nothing from app/infra
- `domainentitynew` — domain entities constructed only via `New<Entity>()`
- `compositeidentity` — every `Save()` carries the 3-tuple identity
- `wireclean` — port methods accept serialisable types only
- `auditshape` — audit-log entries match a schema
- `embedmigration` — guards the 5-phase embedder migration FSM
- `crossdbrefs` — references between Dolt and `_workspace` DBs
- `typedregistry` — plugin slot registration uses typed registry
- `capabilityschema` — slot capability schemas validate
- `slogattrs` — standardised slog attribute names
- `nofmtprintf` — `fmt.Printf` forbidden outside CLI render paths
- `mcpprefix` — MCP tools prefixed with `eng_`
- `sourcelayer` — `Finding.source_layer` ∈ closed enum
- `autolinkconfig` — auto-link config references defined rules
- `riskformula` — risk-score formula uses ratified weights
- `docver` — doc frontmatter `version` matches `last_reviewed`

Most of these protect features that no longer exist in solo
(`compositeidentity` for the 3-tuple, `wireclean` for the wire-typed
plugin registry, `embedmigration` for the 5-phase ceremony,
`crossdbrefs` for the Dolt/`_workspace` split, `typedregistry` and
`capabilityschema` for the typed plugin registry, `riskformula` for
a ratified-weights model). Several others are stylistic
preferences that `golangci-lint` already covers (`slogattrs` via
`sloglint`, `nofmtprintf` via `forbidigo`).

Each custom analyser carries ongoing maintenance — Go SSA
changes break it, false positives need exemption mechanisms, CI
flakes, every contributor learns the rules. The prior set was
unresolved; many analysers protected code that did not yet
exist.

## Decision

V2.0 ships **one custom Go analyser**: `layercheck`.

`layercheck` rule:

> No package under `internal/core/domain/...` may import any
> package under `internal/application/...` or
> `internal/infrastructure/...`.

That is the only invariant we cannot trust to review or to
golangci-lint (no built-in lint enforces import direction across
arbitrary package trees). It catches the one architectural drift
that hurts: domain-on-infrastructure coupling.

Everything else lands on `golangci-lint`:

- `forbidigo` for banned identifiers (e.g., `fmt.Printf` outside
  `cmd/veska/render/`).
- `sloglint` for slog attribute hygiene.
- `gocritic`, `revive`, `staticcheck` for general Go quality.
- `gosec` for security smells.

Naming and shape conventions (`eng_` prefix, EdgeKind closed enum,
`source_layer` enum) are enforced by **types** in the relevant
packages, not by lint:

- MCP tool names: a `RegisterTool` function rejects names that do
  not match the `eng_<verb>_<object>` regex at startup.
- MCP tool freshness class: a typed `Freshness` enum on the
  `ToolSpec` registration struct (SOLO-09 §4.4.1). The unexported
  zero value is invalid; `RegisterTool` panics at startup if any
  tool registers without declaring its freshness, and a contract
  test fails the build for the same case before the binary runs.
- EdgeKind / source_layer: closed Go enum types; the type system
  rejects unknown values at compile time.

If a future invariant cannot be expressed in types or in
golangci-lint, that is the moment to write a second analyser. Not
before.

**Dropped permanently** (these features no longer exist in solo
and need no lint):

- `compositeidentity` (no 3-tuple identity)
- `wireclean` (no typed plugin registry)
- `embedmigration` (no migration FSM)
- `crossdbrefs` (one database)
- `typedregistry` (plain Go interfaces)
- `capabilityschema` (no capability schemas)
- `auditshape` (audit log is unversioned JSONL)
- `autolinkconfig` (auto-link config is a struct)
- `riskformula` (no ratified-weights model in V2.0)
- `docver` (frontmatter is review hygiene, not an invariant)

## Consequences

Positive:

- One analyser to maintain. CI stays fast.
- Type-enforced conventions catch violations at compile time, not
  during a separate lint pass.

Negative:

- Conventions outside `golangci-lint`'s coverage and outside the
  type system rely on review.
- A future server tier may want some of the dropped analysers
  back; revive from Git history at that point.

## Alternatives Considered

- **Keep all fourteen analysers.** Fourteen sources of CI flake
  for invariants that, in solo, are either type-enforced or
  not-applicable. Rejected.
- **Drop all custom analysers including `layercheck`.** Fails the
  "core domain imports nothing from infrastructure" charter pillar
  in practice; review alone has not held this line in V1.
- **Replace `layercheck` with module-level `import` restrictions
  via `go mod` `internal/`.** Standard `internal/` only restricts
  packages outside the module; it does not enforce import
  direction within the module. Insufficient.

## References

- SOLO-07 (architecture — layering rules)
- ADR-S0002 (hexagonal — `layercheck` is the one analyser that
  enforces the layering)
