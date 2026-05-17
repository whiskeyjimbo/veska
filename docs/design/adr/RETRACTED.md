# Retracted ADRs

The solo redesign retracts the following ADRs from the prior V2
set. Each retraction has a one-paragraph rationale. The prior-set
ADR files (ADR-0001..0023, the old non-`S` numbering) are **not
present as files** anywhere under `docs/` — they survive only in
git history. This file is the authoritative list of "these
decisions no longer apply to V2.0".

This document records **nine retraction entries**: eight ADRs are
fully retracted, and ADR-0019 is partially retracted (its
durable-queue decision carries forward; see below).

---

## ADR-0008 — Transport & Auth for Networked Modes

**Retracted.** The solo product has one transport: Unix-domain
sockets — `~/.veska/cli.sock` for CLI traffic and
`~/.veska/mcp.sock` for editor / MCP traffic. The accepting
listener determines `actor_kind` (SOLO-10 §1.2), so the trust
boundary is the file the client connected to. There is no TCP
listener, no TLS, no token auth, no OIDC. The OS file-permission
bits on each socket (mode `0600`) are the entire auth story.

The prior ADR specified TLS modes, token formats, and an OIDC
integration sized for `[W]` and `[C]` tier deployments. None of
those tiers exist in solo (see ADR-S0009). When and if a server
tier is built, transport and auth are part of that fresh design
exercise; they are not extensions of solo's Unix socket.

---

## ADR-0009 — Executor Slot Cardinality → Additive

**Retracted.** There is no executor port at V2.0. The prior ADR
addressed how many concurrent execution ports a worker fleet would
expose, with rules for cardinality and saturation. Solo has no
worker fleet — the daemon is one process with goroutines, sized
by physical cores.

Concurrency in solo is bounded by:

- The promotion transaction (one writer at a time, by SQLite).
- One goroutine per post-promotion queue `work_kind` (see ADR-S0004).
- The embedder's internal worker pool (sized to local Ollama
  throughput).

Those bounds are not "ports" and do not need ratification; they are
implementation choices that can move without an ADR.

---

## ADR-0014 — Typed plugin registry with wire-typed Capabilities

**Retracted; superseded by ADR-S0002.** Plugin ports are plain Go
interfaces in `core/ports/`. The composition root in
`cmd/veska-daemon/main.go` wires impls. There is no registry
type, no `Capabilities` wire format, no version-resolution logic,
no `wireclean` lint analyser.

The prior ADR was justified by a roadmap of multiple impls per
port at multiple tiers. Solo has one impl per port. A second impl
earns its abstraction with a fresh ADR at the time, not now.

---

## ADR-0015 — Embedder migration: dual-write, shadow re-embed, atomic cutover

**Retracted; superseded by ADR-S0007.** The five-phase migration
ceremony (`Planned → DualWrite → Shadowing → Cutover → Complete`)
was sized for a multi-tenant server with downtime budgets. Solo's
swap is "stop daemon, drop embedding rows, restart, rebuild in
background". Five SQL lines and a CLI subcommand. No FSM. No dual
writes. No shadow read.

The cost paid during a swap is hours of degraded semantic search
on a single laptop. The user can read progress in `veska doctor`
and the daemon flips back to full health when the rebuild finishes.

---

## ADR-0018 — Cross-DB attribution refs are eager, not eventual

**Retracted; superseded by ADR-S0005.** The prior ADR built an
eager-cache mechanism for resolving the
`CompositeIdentity{acting_as, on_behalf_of, via}` 3-tuple across
the per-repo DB and the `_workspace` people table. Solo has no
3-tuple. Solo has `actor_id TEXT` and `was_human INTEGER` written
synchronously on every row. There is nothing to attribute eventually
because there is nothing cross-database to attribute.

---

## ADR-0019 — Embedding queue lives in Dolt

**Partially retracted.** The substantive decision — "the embedding
queue is durable, not in-memory" — carries forward, but the queue
lives in SQLite (the `post_promotion_queue` table with `work_kind = embed`),
not in Dolt. The prior ADR also specified a "unresolved drop"
behavior: if the queue grew past a threshold, oldest rows would be
silently dropped. That was a bug, not a design.

**The bug fix:** there is no unresolved drop. The embedding
goroutine applies **backpressure** instead. If the post-promotion queue grows
past a configurable high-water mark, the promotion transaction blocks
on insert until the goroutine drains below the low-water mark.
Promoting is briefly slower; nothing is silently lost. This matches
SOLO-01's "we do not drop user data to keep budgets" pillar.

The queue's persistence guarantee, the at-least-once semantics, and
the per-node idempotency key all carry over from the prior ADR.

---

## ADR-0011 — `finding-revalidator` port and escalation order

**Retracted.** Revalidation is a goroutine that drains
`post_promotion_queue` rows with `work_kind='revalidate'` (SOLO-11 §6),
not a plugin port. The port framing in the prior ADR — `additive`
cardinality, `Uncertain` verdict, deterministic-before-LLM
escalation order, lint enforcement of the order, per-impl trust
gates — was sized for multi-impl revalidator stacks that solo
does not have.

**What carried forward:** the substantive optimisation worth
keeping is the temporal short-circuit. When a finding's anchor
symbol has the same `content_hash` at the current commit as it
had at `detected_commit`, the rule's premise is unchanged and the
rule does not need to re-run. SOLO-11 §6 folds this in as a
pseudocode line above "re-run the rule"; it is an optimisation,
not a separate impl.

**What was cut:** agent re-prompt revalidators (overlapping with
M5's optional review pipeline), the `Uncertain` escalation
machinery, and per-impl gate granularity (solo's gate is
`severity >= high && was_human`).

---

## ADR-0012 — `AuditRun` identity and resume

**Retracted.** Solo's audit log is `audit.jsonl` — append-only,
synchronous before write returns, rotated at 100 MiB keeping five
files (SOLO-08 §3.5, SOLO-10 §4). There is no per-run aggregate,
no resume semantics, no audit-run identity. SOLO-04 §2.1 calls
this out explicitly: *"The audit log is `audit.jsonl`. It has no
per-run aggregate."*

The prior ADR sized run-identity and resume for a multi-tenant
audit-review workflow that solo does not have. A single user
reading their own JSONL file does not need a run aggregate to
correlate events; `actor_id` and `was_human` on every line do
that work.

---

## ADR-0023 — Branch-per-Git-branch policy

**Retracted; superseded by ADR-S0001.** Solo has no Dolt branches.
The graph stores `branch TEXT` as a column on `nodes`, `edges`,
`findings`, and `post_promotion_queue`. Switching Git branches triggers a
tree-sitter rescan into staging; the next promotion writes rows with
the new `branch` value.

The prior ADR specified three reapers (active, idle, deleted) and
a 5,000-branch ceiling sized for canonical-tier deployments.
Solo has one reaper sweep (`veska gc --branches`) that runs
nightly and removes rows whose `branch` no longer exists in
`git branch -a` (with a 7-day grace). No ceiling. No replication.
No three-tier reaper coordination.

---

## Summary

The retracted ADRs share one trait: they were sized for a server
tier that solo does not build. Each retraction is paired with a
solo ADR that handles the genuine concern (identity, embedder
swap, branches, plugin shape) at single-user scale.

If a future server tier is ever built, these retracted ADRs are a
useful starting point — but they are inputs to a fresh design, not
constraints inherited from solo.
