---
id: ADR-S0003
title: Save-vs-promote split with volatile staging
status: accepted
date: 2026-05-08
deciders: [whiskeyjimbo]
---

# ADR-S0003 — Save-vs-promote split with volatile staging

> **Terminology note.** This ADR was originally titled
> "Save-vs-seal split." The body has been rewritten in the new
> *Promote/Promotion* vocabulary per ADR-S0012; the substance of
> the decision is unchanged. The file slug was renamed
> (`save-vs-seal.md` → `save-vs-promote.md`).

## Context

Engram parses files on every save event (fsnotify). V1 prototypes
that wrote to durable storage on each save caused write
amplification and post-commit hook latency spikes. The hot path
must stay under 100ms p95 if Engram is to feel like an editor
extension, not a pipeline.

The save event and the commit event have different cost envelopes:

- **Save** is frequent, fast, and best-effort. The editor still
  owns the buffer; staged graph state can be lost without losing
  user data.
- **Commit** is the unit of consistency. Git already committed; the
  graph just needs to catch up atomically.

We want to keep durable writes off the save path entirely.

## Decision

Separate save from promotion:

- **Save.** Any interactive write (editor save via fsnotify, agent
  edit via MCP, `engram index`) writes only to an in-memory
  `StagingArea` in the application layer. Saves do not touch SQLite.
  StagingArea is volatile by design: no WAL, no persistence across
  daemon restart.
- **Promotion.** Triggered by the post-commit Git hook (or `engram promote`
  for headless runs). Promotion runs one SQL transaction:
  `BEGIN IMMEDIATE`, promote staging deltas to `nodes`/`edges`,
  insert `post_promotion_queue` rows for async work, `COMMIT`. The hook
  returns to Git as soon as the commit lands. Async drain runs in
  goroutines (see ADR-S0004).

`NodeID` is a stable hash of `(repo, language, symbol_path)` so that
re-parses after restart are idempotent: the same content produces
the same row.

MCP read tools default to a staging-overlay-on-promoted view.
Non-editor-loop tools (e.g., audit-shaped queries) take an explicit
`staging: false` to read promoted state only. Both paths are exposed;
the default tracks editor expectations.

Crash recovery: on daemon restart, staging is empty. fsnotify replays
on file-watcher reconnect; tree-sitter re-parses dirty files into
fresh staging. During that window, MCP responses carry
`degraded_reasons: ["staging_recovering"]`.

## Consequences

Positive:

- Hot path stays off durable storage. Hook return p95 is bounded by
  parse time + one SQLite transaction, not by embedding or
  auto-link.
- Commit is the unit of consistency, which matches Git semantics.
- Concurrent reads from MCP tools never block the promotion writer (WAL).

Negative:

- Staged edits not yet promoted are lost on daemon crash. Acceptable
  because the editor still owns the file content; re-saving the
  file or running `engram index` rebuilds staging.
- Two read modes (with-staging and promoted-only) is one more
  parameter for MCP tools to carry. The default is sane and
  documented per-tool.
- No WAL for staging, by design. If we ever want crash-survivable
  staging, that is an ADR.

## Alternatives Considered

- **Write to durable storage on every save.** Rejected: write
  amplification, latency on the hot path, and conflicts with the
  post-promotion queue model.
- **WAL for staging.** Rejected: complexity for a transient buffer
  that the editor and fsnotify can rebuild in under a second.
- **Eventual consistency with no promotion point; rely on a periodic
  sweep.** Rejected: Git commits are the user's mental model for
  "this state matters." Sweeping past them would lose the
  alignment.

## References

- SOLO-01 §5 (the promote-vs-save split as a charter pillar)
- SOLO-08 §5 (the promotion transaction)
- SOLO-11 (pipelines)
- ADR-S0004 (the post-promotion queue that runs after promotion)
