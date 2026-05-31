---
id: ADR-S0002
title: Hexagonal layering, one impl per port at V2.0
status: accepted
date: 2026-05-08
deciders: [whiskeyjimbo]
verified: true
verified_date: "2026-05-16"
---

# ADR-S0002 — Hexagonal layering, one impl per port at V2.0

## Context

V1 mixed application orchestration and infrastructure detail in the
same packages. Tests had to spin up Dolt and Qdrant. Refactors that
should have been local rippled across the tree because the domain
imported the storage clients.

We want the core domain to stay testable with `go test` and zero
external processes. We also want the freedom to swap a port (e.g.,
the embedder, the LLM generator) without rewriting the application
layer.

The prior V2 design carried hexagonal forward but layered on a
typed plugin registry, capability schemas, multiple lint
analysers, and a 12-slot platform contract — most of which
existed to support a future server tier with multiple impls per
port. None of that machinery has a present consumer.

## Decision

Veska Solo is hexagonal / ports-and-adapters with DDD-lite
(entities, aggregate root, repository interfaces, application
service). The non-negotiables:

1. `internal/core/domain/` imports nothing from
   `internal/infrastructure/` or `internal/application/`. Coupling
   flows inward through `internal/core/ports/` interfaces.
2. Application services orchestrate; they do not parse, query
   storage, or call HTTP.
3. Infrastructure adapters implement port interfaces. One adapter
   per port at V2.0. A second adapter is an ADR.
4. The composition root is `cmd/veska-daemon/main.go` and
   `cmd/veska/main.go`. Manual DI; no DI framework.
5. Ports speak in domain types or simple serialisable shapes. No
   port method takes an `io.Reader` whose contents are
   adapter-specific.
6. Domain entities are constructed via `New<Entity>(...required, ...Option)`
   with functional options. Public field assignment of optional
   fields is forbidden.

What we do **not** carry from the prior design:

- No typed plugin registry. Slots are plain Go interfaces in
  `core/ports/`. The daemon wires impls at startup.
- No capability schemas. If a port has optional behavior, it
  exposes a `Capabilities() Set` method returning a closed enum.
- No `wireclean` lint analyser (see ADR-S0010).
- No multi-impl machinery (factory selection, alias resolution,
  versioned port shapes).

Layer enforcement is one custom analyser: `layercheck`. It fails
the build on a domain-package import of `infrastructure/` or
`application/`. That is the entire architectural lint surface at
V2.0.

## Consequences

Positive:

- Domain tests are pure, fast, and require nothing on `$PATH`.
- Adapter swaps stay local. The Ollama embedder can be replaced by
  a hosted-API embedder without the application layer caring.
- The composition root is the one place where wiring lives, which
  makes "what implements what" trivial to read.

Negative:

- We give up the abstraction-up-front gains a typed registry would
  provide if we ever ship a second impl. We accept that cost; the
  second impl earns its abstraction with an ADR at the time, not
  unresolvedly now.
- A port shape that turns out to be wrong gets revised in place
  rather than deprecated alongside its successor. That is the
  right tradeoff for a single-impl world.

## Alternatives Considered

- **Clean Architecture with use-case-per-package.** More ceremony
  than a one-developer codebase warrants. Hexagonal already gives
  us the inversion we need.
- **Typed plugin registry (prior ADR-0014).** Justified only if a
  second impl is on the near roadmap. None is. Retracted; see
  RETRACTED.md.
- **No layering enforcement, rely on review.** Tried in V1; failed
  in V1. The `layercheck` analyser is cheap and catches the only
  import direction that matters.
