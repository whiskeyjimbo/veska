---
id: SOLO-01
title: "Scope and principles — Veska"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-03, SOLO-04, SOLO-08, SOLO-11]
---

# SOLO-01 — Scope and principles

## 1. What Engram is

Engram is a **single-process daemon** that turns local Git
repositories into a queryable graph and a queryable vector index,
and exposes both through MCP. It runs on the developer's laptop.
The data is one SQLite file plus a handful of supporting files
under `~/.veska/`.

**Single-user, not single-contributor.** Engram has one operator —
the developer running the daemon — but it expects to live inside
team repositories with many contributors, CODEOWNERS files, and
shared issue trackers. "Who owns this?" and "who else is touching
this file?" are first-class questions even though there is exactly
one Engram user on the machine.

**Multi-repo, single-daemon.** The daemon indexes N repos
simultaneously — a service, its SDK, a docs repo are the typical
solo working set. Repos are dynamic state: `veska repo add`
registers; `veska repo remove` reverses. Cross-repo edges (service
A `CALLS` SDK B) are computed at query time, not stored — see
SOLO-04 §5.4 and SOLO-11 §9. Cross-repo answers always reflect the
target repo's current promoted branch; there is no version-pinning.
Resource ceilings (RSS, FDs, goroutines) are daemon-global, not
per-repo.

The system has three parts:

1. **Substrate.** The graph (nodes, edges) and the embeddings,
   stored in SQLite + sqlite-vec, in one file.
2. **Pipelines.** What runs on save (in-memory only) and what runs
   on promotion (commit-time, durable).
3. **MCP.** The editor- and agent-facing API. SOLO-09 §3 is the canonical inventory.

There is no "platform contract" part because the platform
has one consumer (this daemon), one user (the developer), and one
impl per port.

## 2. Mission

Give one developer and their one AI agent the same grounded model
of the codebase, with no SSO setup, no upstream service, and no
infrastructure to operate.

## 3. Non-goals

Deliberate non-goals:

- **Multi-machine deployment.** No replication, no canonical tier,
  no worker fleet. If a future ADR brings it back, it lives under
  `deferred/` until then.
- **Multi-tenant isolation.** One user, one daemon, one machine.
- **Full audit/compliance.** Append-only `audit.jsonl` is the
  whole story. Forward it elsewhere if you need more.
- **Visual UI.** Editor surfaces own visualization.
- **LLM-as-primary-truth.** Tree-sitter and the parser are the
  primary truth for structure. LLMs review and summarise.
- **Distributed graph storage.** SQLite handles a 1M-node graph on
  a laptop. If we need more, that's an ADR.
- **Identity-as-a-product.** "The user who started the daemon" plus
  one enum (`actor_kind`: `human | agent | system`; SOLO-10 §1.2).
  No OIDC, no SCIM, no SAML. Not now, not later (those return only
  if a server tier is built).

## 4. Principles

1. **Core domain imports nothing from infrastructure.** All
   coupling flows inward through `core/ports/` interfaces.
2. **One impl per port at M1.** Each port ships exactly one
   implementation when M1 lands. A *second* impl is the bar that
   needs an ADR; once it lands, provider-keyed selection
   (`[<port>].provider = "x" | "y"`) is the legitimate
   composition mechanism — no capability schemas, no typed
   registry, no slot lint, but also no pretence that adding a
   `provider` switch needs ceremony beyond the ADR that justified
   the second impl. SOLO-05 §1.1 lists the four ports that
   already carry a `provider` key.
3. **Save is in-memory; promotion is the synchronous durable
   write.** Interactive saves never touch the disk. The
   synchronous SQLite write path runs only at `post-commit` (the
   promotion transaction publishes nodes/edges and enqueues
   `post_promotion_queue` rows in one `BEGIN IMMEDIATE`). The
   asynchronous post-promotion drains (`embed`, `auto_link`,
   `revalidate`, `review`) also write durably — they have to,
   because findings and embedding rows are persistent state — but
   only *after* the promotion transaction commits, and never on
   the user's commit-return critical path. The pillar is "save
   never blocks on disk; the user's `git commit` blocks only on
   the bounded promotion transaction." See SOLO-11 §2 (promotion
   pipeline) and §10 (writer-pool serialisation).
4. **Measure before budget.** Performance numbers live in
   SOLO-13 §3 under a labelling convention: every number is
   `BUDGET (unmeasured)` / `BUDGET (measured M<N>)` / `INVARIANT`
   / `DEFAULT`. Unmeasured budgets cannot be cited as decisions.
   Other docs cross-reference SOLO-13 instead of repeating
   numbers.
5. **The CLI is the operator surface.** `veska doctor` is what
   the user sees. Spec it before its observability backend.
6. **Deferred work stays out of normative text.** Anything not
   shipped lives under `deferred/` and is not referenced by
   normative sections.
7. **No future-proofing.** The first impl is the only impl. Don't
   shape interfaces for tiers that don't exist.

## 5. The promote-vs-save split

The single most important design decision: saves write only to
in-memory staging; only `git commit` (via the post-commit hook)
promotes to SQLite. The full mechanism lives in **SOLO-11**
(canonical) and **ADR-S0003** (rationale). The read merge between
staging and promoted state is per-file overlay, not row-level merge
(SOLO-11 §1.2). History-rewriting Git operations (rebase, merge
continuation, cherry-pick, bisect, amend) are handled by SOLO-11
§2.3 — short-circuit during the operation; catch-up via the
divergent-promotion path afterward. Promotion never blocks on post-promotion
queue depth (SOLO-08 §3.4); embedding saturation surfaces as a
sticky finding rather than back-pressuring the hot path.

## 6. Naming

Known collisions:

- **"Engram"** collides with the keyboard layout, the Engram
  memory-engineering startup, and several neuroscience products.
  The binary is `veska`, the Go module is `veska`, the data dir
  is `~/.veska/`. For a personal, on-device product, discoverability
  is not the goal and the collision is acceptable. **Trademark
  resolution is required before any V2.1 server-tier work begins**
  — that is the deadline, not "deferred indefinitely." A server
  tier introduces marketing surface and a public registration
  story that the current name cannot carry without a check.
- **"Beads"** (the default tracker integration) collides with a JS
  framework and the venerable `bd` directory-bookmarking utility.
  In docs and user-visible surfaces, the integration is "the
  tracker" or "the bd-cli tracker" when specificity matters.
  Config uses `[tracker] provider = "bd-cli"`; actor IDs are
  `human:<user>` and `agent:<llm-name>`, never `beads:*`. Same
  V2.1 deadline applies if the integration becomes user-facing
  beyond the current opt-in shape.

Vocabulary inside the docs uses plain names. "post-promotion queue drain" is
a goroutine reading rows from the `post_promotion_queue` table, not a
saga. "Port" is a Go interface in `core/ports/`. "Repo" is a Git
repository the daemon has registered; "session" is one daemon
process lifetime. There is no "workspace."

## 7. Vocabulary (just enough)

| Term | Meaning |
|---|---|
| `Node` | A symbol (function, type, file, package). |
| `Edge` | A relation between nodes (calls, imports, tests, etc.). |
| `Graph` | The set of nodes and edges, scoped by `(repo, branch)`. |
| `Task` | A unit of work, often pinned to a tracker issue. |
| `Finding` | A surfaced concern (vuln, drift, dead code, etc.). |
| `Suppression` | A user/agent decision to silence a finding. |
| `Actor` | The originator of a state-changing operation. `actor_kind` is `human`, `agent`, or `system` (SOLO-10). The whole identity model. |
| `Promote` | The atomic write of staging → SQLite on commit. |

SOLO-15 is the authoritative glossary.

## 8. Status

Draft. Nothing is approved. Approval comes after M1 ships and the
measurements above are real numbers.
