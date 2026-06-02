---
id: ADR-S0019
title: "Shared-store port + idempotent merge-record model (transport deferred to adapter)"
status: Proposed
date: 2026-06-02
deciders: [whiskeyjimbo]
supersedes: []
extends: [ADR-S0018]
related: [ADR-S0017, ADR-S0018, ADR-S0009, ADR-S0007, ADR-S0015, ADR-0020]
verified: false
---

# ADR-S0019 — Shared-store port + idempotent merge-record model

[[ADR-S0018]] deferred two things: the sharing **transport** and the **merge
policy**. This ADR settles them — by deciding *not* to pick a transport. The
durable decisions are a **port** for the shared store and a **merge model that
lives in the records**, not in any transport. Concrete transports become
swappable **adapters**, exactly as `VectorStorage` (memvec/usearch,
[[ADR-S0015]]) and `EmbeddingProvider` (Ollama/model2vec, [[ADR-S0007]]) already
are.

## Status

Proposed. Extends [[ADR-S0018]]. The transport adapter itself is intentionally
left open (see _Deferred_).

## Context

The design nearly committed to Dolt as the transport, on the argument "Dolt is
already in-stack." That argument was a category error: **Dolt is _beads'_ choice
— the issue tracker we use to build veska — not part of veska's own
architecture.** veska has no reason to inherit a dev-tool's storage engine.

Stepping back, the project is ports-and-adapters throughout (`layercheck`
enforces inward coupling). We were debating *adapters* (Dolt vs. git-JSONL)
before specifying the *port*. That is backwards, and it is the source of the
stall: a transport cannot be chosen well without first knowing the contract it
must satisfy. So this ADR defines the contract and the record model, and treats
the transport as a later, low-risk adapter swap.

## Decision

### 1. The shared store is a port; the transport is an adapter

Define consumer-owned port(s) for the shared store, sized to what their
consumers need — not a transport-shaped interface. The merge model (§2) makes
the contract small: an adapter only has to **exchange sets of records**.

Likely two ports, because the two payloads have different access patterns (this
is an adapter-granularity detail, not a new merge model):

- **`SharedCurationStore`** — small, mutable, human-authored class-B records:
  roughly `Publish(records)` / `Pull(since cursor)`.
- **`SharedArtifactStore`** — content-addressed, immutable, bulk artifacts
  (embeddings + other expensive derived output): `Put(hash, blob)` /
  `Get(hash)`; semantics are a pure key→blob union.

Candidate adapters — git-tracked export, a shared directory, an object store, a
peer daemon, or Dolt — all satisfy these. Ship the simplest first; swap with no
redesign.

### 2. Merge correctness lives in the records, not the transport

This is where the "idempotency + version fields" instinct belongs — as a
property of the **domain record**, so that *any* adapter that exchanges record
sets merges correctly:

- **Idempotency key** — a content-derived id, so two contributors making the
  same decision produce the *same* record → union dedups it.
- **Version** — a logical clock (Lamport-style counter, not wall-clock) on
  mutable records, so concurrent edits resolve by a **deterministic fold**:
  last-writer-wins on `(version, actor)`, with prior versions preserved as
  attributed history. (Accepts LWW's lost-update on a genuinely concurrent
  same-field edit; the loser survives in history. Sufficient for a few
  contributors; richer CRDTs are not warranted — relates to [[ADR-0020]].)
- **Actor** — attribution + the LWW tiebreak.

Because correctness is in the data, the transport stops being the hard problem —
which is *why* we can defer it.

### 3. Two record families, one merge model

| Family | Records | Merge |
|--------|---------|-------|
| **Class-B curation** | `suppressions`, finding triage (close/reopen + reason + actor) | idempotent + versioned → deterministic fold (§2) |
| **Expensive derived artifacts** | embeddings, **summaries/condensations** ([[ADR-S0018]] §4; `oo4q`), model-generated **review** output | content-addressed by `(input_hash, model, prompt/template_version)` → immutable → **union** (the degenerate case of §2) |

The artifact family is the generalisation of "share embeddings": **share by
*production cost*.** Anything produced by an LLM/model call is expensive to
recompute and deterministic enough to content-address, so it is both worth
sharing and trivially conflict-free. Version-homogeneity ([[ADR-S0018]] §3)
plus **consume-don't-recompute** (if the shared store already has the hash,
pull it rather than re-running the model) preserves the content-addressed
invariant across non-bit-deterministic hardware.

### 4. Share-vs-regenerate is set by production cost — and measured, not guessed

[[ADR-S0018]] §4 established the axis; this sharpens the rule:

> Share what is **expensive to produce** (LLM/model artifacts) or
> **irreproducible** (class B). Regenerate locally what is **cheap and
> deterministic** (parse-derived nodes/edges/FTS/imports).

The cheap/expensive line is **deployment-dependent**: with a cheap CPU embedder
(model2vec / hash-static) even embeddings may be cheaper to regenerate than to
sync; with heavy Ollama/nomic they are clearly worth sharing. So it is a
per-deployment toggle behind `SharedArtifactStore`, and the boundary is set by
**measurement**:

- **Empirical gate (`solov2`-tracked):** on real large libraries, measure the
  delta — local regeneration cost (cold-scan + embed + summarise + review) vs.
  the size/sync cost of carrying each artifact in the shared store. Place the
  share/regenerate line per artifact from data, not assumption.

## Deferred

- **The concrete transport adapter.** Chosen later, informed by the empirical
  gate. Candidates above; a networked/canonical adapter would reopen
  [[ADR-S0009]] and gets its own ADR.
- **Class-B conflict UX** beyond deterministic LWW (e.g. surfacing a contested
  triage to a human) — extends [[ADR-0020]].

## Consequences

- The contested Dolt-vs-git decision is **removed from the critical path** — it
  is now an adapter choice, deferrable and reversible.
- The merge policy is engine-independent and testable in isolation (fold over
  records), with no transport stood up.
- Adding a transport later is a single adapter, consistent with the existing
  dual-backend precedents ([[ADR-S0007]], [[ADR-S0015]]).
- [[ADR-S0017]] remains the precondition: converged `node_id` / content hashes
  are the keys the records and artifacts are addressed by.
- A measurement task gates the share-vs-regenerate boundary, so the shared
  store carries only what the data says is worth carrying.
