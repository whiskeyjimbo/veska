---
id: ADR-S0004
title: One post_promotion_queue table, four work-kinds, one goroutine each
status: accepted
date: 2026-05-08
deciders: [whiskeyjimbo]
verified: true
verified_date: "2026-05-16"
---

# ADR-S0004 - One post_promotion_queue table, four work-kinds, one goroutine each

> **Terminology note.** This ADR was originally titled
> "One `post_seal_queue` table…" The body has been rewritten in
> the new *Promote/Promotion* vocabulary per ADR-S0012; the
> substance of the decision is unchanged. The file slug was
> renamed (`post-seal-queue-table.md` →
> `post-promotion-queue-table.md`).

## Context

After the promotion transaction commits, several pieces of follow-up
work need to happen: generate embeddings for new/changed nodes,
run auto-link to refresh findings, revalidate prior findings
against the new graph, optionally run an LLM review pass. These
must not block the post-commit hook.

The prior design called this an "post-promotion queue saga" with "per-stream
lanes", a "SealCoordinator", and explicit head-of-line-blocking
mitigations. That vocabulary, and most of the machinery, was sized
for a server tier with cross-database fan-out (Dolt graph DB,
`_workspace` content store, embedding queue in a third place). In
the solo product all four work-kinds run inside one process against
one SQLite database. The "saga" is a goroutine reading rows.

We want the post-promotion queue to be:

- One table, queryable from `veska doctor`.
- Recoverable on restart without manual intervention.
- Bounded in size (no unbounded growth on a long-running daemon).
- Self-acknowledging on persistent failure (a single-user product
  cannot rely on the user running `veska post-promotion queue ack`).

## Decision

One SQLite table, `post_promotion_queue`, holds all post-promotion work:

```
seq           INTEGER PRIMARY KEY AUTOINCREMENT
promotion_id       TEXT  NOT NULL  -- ULID per commit
repo_id       TEXT  NOT NULL
branch        TEXT  NOT NULL
git_sha       TEXT  NOT NULL
work_kind     TEXT  NOT NULL  -- embed | auto_link | revalidate | review
payload       TEXT  NOT NULL  -- JSON
state         TEXT  NOT NULL  -- pending | in_progress | done | failed
attempts      INTEGER         DEFAULT 0
enqueued_at   INTEGER NOT NULL
completed_at  INTEGER
error         TEXT
```

Four `work_kind` values, exactly:

- `embed` - generate embeddings for new/changed nodes.
- `auto_link` - refresh findings against the new graph.
- `revalidate` - recheck open findings still apply.
- `review` - optional LLM review pass; off by default at V2.0.

One goroutine per `work_kind`. Each polls
`SELECT ... WHERE state = 'pending' AND work_kind = ? ORDER BY seq LIMIT 16`
at **250ms** cadence. There are no per-stream "lanes" beyond the
`work_kind` column. Goroutines are independent; head-of-line
blocking within a kind is acceptable because the kinds are
independent already.

Retries: 3 attempts with exponential backoff (1s, 4s, 16s). On the
fourth failure, `state = failed` and `error` is preserved.

**Per-`work_kind` failure policy.** Failed rows stay `failed`.
There is no 24h auto-acknowledge sweeper - the prior draft of
this ADR specified one but it had a fatal flaw: silently flipping
`failed → done` after a day means an `embed` row whose model
config is permanently broken disappears from the doctor surface
without ever producing a vector, and the only signal
(`degraded_reasons: ["embedding_pending"]`) keys on `state =
pending`, so a `done` row gives no degraded signal at all. The
failure becomes invisible. We replaced it with a uniform "stays
failed; user retries" model:

| `work_kind` | When the row hits `state = failed` | Rationale |
|---|---|---|
| `embed` | Row stays `failed`. Reads against affected nodes carry `degraded_reasons: ["embedding_failed"]` (distinct from `embedding_pending`). User retries via `veska doctor post-promotion-queue retry --kind=embed [--seq=N]`. | Embed failures are usually a model config or pull issue; user is in the loop already, automatic retry hides the cause. |
| `auto_link` | Row stays `failed`. Diagnostic only - the next promotion re-runs auto-link over the affected nodes, so the failure self-heals on subsequent activity. | Suggestion-shaped; one missed run is not load-bearing. |
| `revalidate` | Row stays `failed`. The hourly sweep (SOLO-11 §6) re-evaluates every open finding regardless of post-promotion queue state. | Backstop is independent of the post-promotion queue row. |
| `review` | **Sticky with a finding.** The daemon emits a `Finding` with `source_layer='quality'`, `severity='high'`, `rule='review-pipeline-failure'`, anchored to the promotion's commit. The post-promotion queue row stays `failed` until that finding closes through the human-action gate; closing flips the row to `done`. | No backstop; user with `review.enabled=true` believes review ran. |

Closing the review-pipeline-failure finding trips the standard
human-action gate (`severity >= high` requires `actor_kind=human`), so a
human acknowledgement is recorded in `audit.jsonl` before the
failure is forgotten. No new operator surface; the human-action-gate
machinery already designed handles it.

**Retry surface.** `veska doctor post-promotion-queue retry [--kind=K] [--seq=N]`
moves matching `failed` rows back to `pending`, zeroes `attempts`,
clears `error`, and lets the drain pick them up. Without flags it
retries every `failed` row. The `review` kind is included only
when explicitly named (`--kind=review`) because its sticky-finding
machinery exists for a reason; bulk-retrying review failures
skips the human-action-gate ack.

**Backpressure on queue depth.** Independent of per-row failure
handling: when `post_promotion_queue` row count exceeds `post_promotion_queue.high_water`
(configurable; default 10 000), the promotion transaction blocks on
post-promotion queue insert until the drain goroutines bring depth below
`post_promotion_queue.low_water` (default 8 000). Promoting is briefly slower;
nothing is silently dropped. This replaces the prior design's
"unresolved drop" behaviour (see RETRACTED.md, ADR-0019).

Garbage collection: rows with `state = done AND completed_at < now() - 7d`
are deleted by the same sweeper.

There are no compensating actions and no inverse operations. A
`failed` row stays failed (or becomes a finding, for `review`);
the user sees the state via `veska doctor post-promotion-queue` and the
daemon does not block on it.

## Consequences

Positive:

- One table, one set of queries, one mental model.
- Restart recovery is a `SELECT WHERE state IN ('pending', 'in_progress')`;
  rows in `in_progress` get reset to `pending` on startup.
- Failures are visible. `embed` failures surface
  `degraded_reasons: ["embedding_failed"]`; everything else
  surfaces in `veska doctor post-promotion-queue`. Nothing is silently dropped.
- `veska doctor` has a one-query health view of all post-promotion work.

Negative:

- An `embed` row that fails persistently (e.g. embedder model
  file corrupted) stays `failed` until the user retries. The
  affected nodes have no embedding; semantic-search responses
  carry `degraded_reasons: ["embedding_failed"]`. The user reads
  `veska doctor post-promotion-queue`, fixes the cause, runs
  `veska doctor post-promotion-queue retry --kind=embed`. We accept the manual
  step in exchange for never silently dropping work.
- A `review` row that fails persistently emits a finding. This
  adds one rule to the codebase (`review-pipeline-failure`) and
  one new way for the human-action gate to be invoked. Worth it: silent
  review misses would defeat the reason for having the gate at all.
- Within `work_kind = embed`, ordering is per-`seq`. A slow row at
  the head delays its peers. Mitigation: the embedder uses a
  bounded internal worker pool so a single slow node does not
  starve the others within a batch.

## Alternatives Considered

- **Per-lane post-promotion queue tables.** Adds tables for no benefit; the
  `work_kind` column gives the same physical separation via index.
- **Saga with compensations.** Compensations are hard, hard to
  test, and require human reasoning per kind. We do not need them
  in a single-process world. The existing promotion transaction is
  atomic; everything after promotion is a retryable side effect.
- **No table, in-memory queue only.** Loses crash-recovery; a
  daemon crash mid-embed would orphan nodes. Persistence is
  cheaper than recovery sweeps over the working tree.
- **24h auto-acknowledge sweep on `failed`.** Earlier draft of
  this ADR. Rejected on review (see SOLO-08 §3.4): silent
  `failed → done` flipping makes broken embed configs invisible
  one day later; the only visibility hook
  (`degraded_reasons: ["embedding_pending"]`) keys on
  `state = pending`, so post-ack rows surface as healthy.
- **Per-row manual ack required for healthy rows.** Fails the
  "no operator surface beyond `veska doctor`" charter rule.
  The retry surface here is for `failed` rows only; `done` rows
  GC themselves at the 7-day mark.

## References

- SOLO-08 §3.4 (post_promotion_queue schema)
- SOLO-11 (pipelines)
- ADR-S0003 (the promotion transaction that writes post-promotion queue rows)
