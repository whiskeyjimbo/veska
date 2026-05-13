---
id: ADR-S0009
title: Server tier is out of scope for the V2.0 doc tree
status: accepted
date: 2026-05-08
deciders: [whiskeyjimbo]
---

# ADR-S0009 — Server tier is out of scope for the V2.0 doc tree

## Context

The prior V2 design was written for a 200–500-developer SaaS with
three tiers (`[L]` local, `[W]` workstation, `[C]` canonical),
multi-tenant isolation, OIDC, replication, worker pools, mode
affinity tags on every pipeline step, and a "future drop-in"
architectural commitment that shaped every Go interface for an
out-of-process loader that didn't exist yet.

Late in the design cycle V2.0 was rescoped to local-only by
adding disclaimers to the existing documents. The disclaimers
named the rescope; the substance — schema columns, lint
analysers, ADR ratifications, MCP tool descriptions, mode-
affinity tags, audit-log shapes, replication contracts — still
encoded the larger system.

The "future drop-in" promise is what kept that substance alive:
if today's port shapes have to support a future server tier, the
abstraction cost lands today (extra columns, generic interfaces,
capability schemas). Drop the promise and the local product
gets to be local.

## Decision

Engram Solo's documentation does **not** specify any of the
following. They are not in the V2.0 doc tree, in any form, normative
or aspirational:

- A server tier, canonical tier, workstation tier, or any tier
  above the developer's laptop.
- Mode tags (`[L]` / `[W]` / `[C]`) on pipelines, ports, or tools.
- Tenants, organisations, projects-as-multi-user-scopes.
- Cross-machine replication, replication streams, or consensus.
- OIDC, SAML, SCIM, or any external identity provider integration.
- Worker pools, executor cardinality models, or queue-based work
  distribution across processes.
- Port shapes designed to accommodate future out-of-process
  loading.

The "future drop-in" principle is **rejected for V2.0**. Ports
are Go interfaces sized for the one impl that ships. A future
server tier is a fresh design exercise — new sections, new
ADRs, new ports — not an extension of the present shapes.

If a feature in the present design only makes sense in a server
context, it is not in the present design. Examples: per-tenant
quotas, replication-aware promotion coordination, cross-tier cache
invalidation, OIDC token refresh budgets.

Anything explicitly deferred — work we may do later but is
out of V2.0 — lives under `docs/docsv2solo/deferred/` and is not
referenced from normative text. Disclaimers in normative text are
not allowed; if the substance is deferred, the substance is moved.

## Consequences

Positive:

- Reviewers read what V2.0 ships.
- Ports stay sized for one impl.
- A future server tier, if it ever happens, gets a fresh design
  without inheriting constraints from the solo docs.

Negative:

- If a server tier ever ships, some of today's port shapes will
  be wrong for it. They get rewritten at the time. Bounded cost
  paid then, instead of unbounded cost paid now for a tier that
  may never exist.

## Alternatives Considered

- **Keep the server-tier substance under disclaimers.** What the
  prior design did. Empirically, the disclaimers did not prevent
  reviewer fatigue or accidental implementation drift toward the
  larger system. Rejected.
- **Move server-tier substance to an `appendix/` directory but
  keep cross-references from normative text.** Halfway. The
  references re-pollute the normative text. Rejected.
- **Build the abstractions; suppress the docs.** Worst of both:
  the code grows tier-aware shapes for no V2.0 user, and reviewers
  cannot see what the code is for.

## References

- SOLO-00 §What we don't write (no "future drop-in" promises)
- SOLO-01 §3 (non-goals)
- SOLO-01 §4.7 (no future-proofing)
- `docs/docsv2solo/deferred/` (where deferred features live)
