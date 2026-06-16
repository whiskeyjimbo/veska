---
id: ADR-S0012
title: Rename "Seal" to "Promote/Promotion" across the design tree
status: accepted
date: 2026-05-09
deciders: [whiskeyjimbo]
verified: true
verified_date: "2026-05-16"
---

# ADR-S0012 - Rename "Seal" to "Promote/Promotion"

## Context

The save/seal split (ADR-S0003) is the load-bearing primitive of
the V2 design: interactive saves write to in-memory staging;
`git commit` triggers a synchronous SQLite transaction that
publishes nodes/edges and enqueues async work. The naming chosen
in the original draft was *Seal* - a deliberately distinctive
verb that didn't collide with Git's "commit."

Cumulative usage made the term unworkable:

- *Seal* was used as a verb ("to seal staging"), as a noun ("the
  seal," "during seal"), and as the root of an adjective form
  ("sealed_sha," "sealed state," "the sealed graph"). One word,
  three grammatical roles, all load-bearing.
- Compounds proliferated: `post_seal_queue`, `seal_id`,
  `seal_pipeline`, `ErrSealDivergent`, `Seal RPC`, `seal
  barrier`, `save-vs-seal`. Each one re-encoded the same
  ambiguity in a different shape.
- The PRODUCT.md narrative had to insert a glossary block
  explaining the three meanings. The fact that the user-facing
  trailer needed disambiguation in the first paragraph was a
  signal - fixed at the source.

The principal-architect review of 2026-05-09 named this as the
single biggest terminology problem in the doc tree:

> Three meanings of one word in the load-bearing primitive of
> the system is unworkable for new readers. The collision the
> design creates with itself is worse than any external
> collision.

## Decision

Rename throughout the SOLO design set:

| Old | New |
|---|---|
| Seal (verb) | Promote |
| Seal (noun) | Promotion |
| Sealed | Promoted |
| Sealing | Promoting |
| Seals (plural) | Promotions |
| sealed_sha | promoted_sha |
| seal_id | promotion_id |
| post_seal_queue | post_promotion_queue |
| post-seal-queue | post-promotion-queue |
| seal_pipeline | promotion_pipeline |
| seal barrier | promotion barrier |
| ErrSealDivergent | ErrPromotionDivergent |
| `veska seal` (CLI) | `veska promote` |
| Seal RPC | Promote RPC |
| save-vs-seal | save-vs-promote |
| PostSealQueueRepository | PostPromotionQueueRepository |
| PostSealQueueDrainer | PostPromotionQueueDrainer |

The verb form is *promote* (active, transitive: "the post-commit
hook promotes staging to SQLite"). The noun form is *promotion*
("the promotion runs in one `BEGIN IMMEDIATE`"). The adjective
form is *promoted* ("the promoted state"). Three forms, three
words, one root - the same separation English already gives us
with commit/committed/committal but without colliding with Git.

The CLI verb is `veska promote` (rare; only used for headless
batch runs and `--retry`). The post-commit hook continues to
fire automatically; users mostly never type the verb.

## Consequences

Positive:

- Every grammatical role gets its own word. New readers stop
  needing the glossary block to parse the first paragraph.
- The `Promotion` aggregate-root naming aligns with the
  vocabulary ADR-S0003 always meant: a *Promotion* is a discrete
  event with a `promotion_id`, a `promoted_sha`, and an audit
  trail; *promoted state* is the durable graph; *to promote* is
  the verb the post-commit hook performs.
- The CLI command `veska promote` reads as the verb it is.
- "Save vs. Promote" reads more naturally than "Save vs. Seal"
  did - both are verbs, parallel grammatical role.

Negative:

- One large mechanical rename across ~37 files. No code exists
  yet, so the cost is bounded to docs.
- Two ADR file names retain their original `seal` slug
  (`ADR-S0003-save-vs-promote.md`, file renamed;
  `ADR-S0004-post-promotion-queue-table.md`, file renamed).
  Earlier git history, retracted ADRs, and prior-set docs
  reference the old names; those are historical and not
  rewritten.
- Anyone reading the prior-set design tree (`docs/design/`,
  preserved until M6 cutover per SOLO-00) will see the old
  vocabulary. That tree is read-only; this rename does not
  touch it.

Neutral:

- `Seal` as a term has been retired from normative text. ADRs
  that originally named the term (S0003, S0004) carry a header
  note pointing at this ADR; their body text is rewritten in
  the new vocabulary so a reader can pick up the doc cold.

## Alternatives Considered

- **Keep `Seal`, tighten the glossary.** Rejected. The glossary
  block was the symptom; the disambiguation was needed in
  user-facing copy, which is where terminology should be
  weakest. Stricter usage rules don't survive the next author
  with no awareness of the rule.
- **Rename to `Commit`/`Committed`.** Rejected. Git owns those
  terms and the design relies on Git commits as the trigger;
  using *commit* for both events guarantees confusion at the
  point most readers are most confused already.
- **Rename to `Persist`/`Persistence`.** Rejected. *Persistence*
  is a generic term in the storage literature and overlaps with
  unrelated concepts (persistent connections, persistent
  volumes). *Promotion* names the act and only the act.
- **Rename to `Land` / `Landing`.** Considered. `Land` reads
  naturally as a verb ("the commit lands in SQLite") and
  doesn't collide. Rejected because the noun form (`Landing`)
  reads worse than `Promotion` for an audit-log row, and the
  past-participle (`landed_sha`) is fine but not better than
  `promoted_sha`. Coin-toss decision; `Promote` won on the
  strength of its noun form.
- **Rename to `Materialize` / `Materialization`.** Rejected.
  Five syllables; reads as a database-internals term; carries
  baggage from materialised-view literature.

## References

- ADR-S0003 - Save-vs-promote split (was: save-vs-seal). Header
  note added pointing to this ADR.
- ADR-S0004 - Post-promotion queue table (was: post-seal queue
  table). Header note added pointing to this ADR.
- SOLO-01 §5 - The promote-vs-save split (renamed in §5 header).
- SOLO-08 §5 - The promotion transaction.
- SOLO-11 §2 - Promotion pipeline.
- SOLO-15 - Glossary (entries updated; "Promote" replaces
  "Seal").
