---
id: SOLO-02
title: "Personas & Stories — Dev and Agent"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-01, SOLO-03, SOLO-09, SOLO-10, SOLO-11, SOLO-12]
---

# SOLO-02 — Personas & Stories

Two personas, ten stories. Each story is two or three lines: who
acts, what happens, what acceptance looks like. Design sections
back-reference stories by their `US-NN.MM` ID.

## 1. Personas

| ID | Persona | Definition | Surfaces |
|---|---|---|---|
| `dev` | **Dev** | The human developer who started the daemon. | `engram` CLI, editor MCP, git hooks |
| `agent` | **Agent** | An AI coding assistant acting via MCP. | `engram-mcp` stdio shim, Unix-socket MCP |

That is the whole cast. There is no SecEng (the Dev reviews their
own findings), no Lead (single user), no Ops (the Dev runs their
own daemon), no SRE (no fleet to be on call for). Those personas
return only if a server tier is ever built; until then they are
not in scope.

## 2. Story ID convention

```
US-NN.MM
   │  └── persona: 01 = Dev, 02 = Agent
   └───── lifecycle moment: 01..07 (see §3)
```

IDs are stable. A dropped story is marked `status: dropped` in
place; its ID is not reused.

## 3. Lifecycle moments

| ID | Moment |
|---|---|
| 01 | First install on a new repo |
| 02 | Daily edit loop |
| 03 | Commit and push |
| 04 | Querying the graph |
| 05 | Findings (surface + suppression) |
| 06 | Branch switching and stale state |
| 07 | Daemon restart |

## 4. Stories

### US-01.01 — Dev: First install on a new repo

**Status:** planned
**Satisfied by:** SOLO-03

The Dev runs `engram init` in a Git working tree. The daemon
starts, the post-commit hook is installed, and a cold scan
populates the promoted graph in the background.

**Acceptance.** `engram init` exits 0 within one second; the
daemon's PID file appears at `~/.engram/daemon.pid`; the cold
scan completes within the cold-scan budget (SOLO-13 §3.2); the
user is not asked to log in, configure YAML, or accept a
network egress prompt.

### US-02.01 — Dev: Daily edit loop

**Status:** planned
**Satisfied by:** SOLO-11

The Dev edits files in their editor. fsnotify picks up each
save; the staging area updates within milliseconds; subsequent
MCP queries from the editor reflect the unsaved edits via the
staging overlay.

**Acceptance.** A save-to-staging-visible round trip honours the
SOLO-13 §3.1 budget; the promoted graph is unchanged (saves do not
write to SQLite); restarting the daemon discards staging and
falls back to promoted state without error.

### US-02.02 — Agent: Daily edit loop

**Status:** planned
**Satisfied by:** SOLO-09, SOLO-11

The Agent calls `eng_find_symbol`, `eng_get_call_chain`, and
`eng_get_dirty_blast_radius` over MCP while the Dev is mid-edit.
Responses include unsaved changes via the StagingArea overlay.

**Acceptance.** Every response carries `included_staging: true`
when staging contributed rows (SOLO-09 §4.4); the agent's view of
the graph matches the editor's view within the same staging
window.

### US-03.01 — Dev: Commit and push

**Status:** planned
**Satisfied by:** SOLO-08, SOLO-11

The Dev runs `git commit`. The post-commit hook fires
`engram promote`, which promotes staging to SQLite atomically and
writes a `post_promotion_queue` row. The hook returns to Git within the
hook-return budget. Async drains run after.

**Acceptance.** Hook return honours the SOLO-13 §3.1 budget
(typical vs. refactor commit split); the promotion transaction is
atomic (a crash mid-promotion leaves staging unmodified); `git push`
is not blocked on embedding completion.

### US-04.01 — Agent: Find a symbol

**Status:** planned
**Satisfied by:** SOLO-09

The Agent calls `eng_find_symbol` with a name and an optional
kind filter. The daemon returns a list of summary projections
ranked by exact-match-then-fuzzy.

**Acceptance.** Warm p95 honours the SOLO-13 §3.1 `find_symbol`
budget; the response respects the per-response token budget; the
response sets `included_staging` per SOLO-09 §4.4.

### US-04.02 — Agent: Compute blast radius for the active task

**Status:** planned
**Satisfied by:** SOLO-09, SOLO-12

The Agent has previously called `eng_set_active_task`. It now
calls `eng_get_dirty_blast_radius` (or `eng_get_context_pack` for
the broader pack). The response includes both promoted edges and
staging-area changes since the task became active.

**Acceptance.** Unresolved edges are excluded by default; the
response is reproducible across two calls within the same staging
window; the agent can opt in to unresolved edges with
`include_unresolved: true`.

### US-05.01 — Dev: Surface a vuln finding from a feed

**Status:** planned
**Satisfied by:** SOLO-05, SOLO-11

The Dev has configured a vuln source. The daemon polls the feed
on its cadence; new findings land as `Finding` rows with
`source_layer: "security"`. The editor's MCP client lists them
via `eng_list_findings`.

**Acceptance.** A new finding appears in `eng_list_findings`
within one poll cycle of the feed publishing it; each finding
carries a stable id and a target `node_id`.

### US-05.02 — Dev: Suppress a false positive

**Status:** planned
**Satisfied by:** SOLO-09

The Dev decides a finding is a false positive. They (or the
agent on their behalf) call `eng_suppress_finding` with a scope
(symbol / file / repo / finding-id) and a reason.

**Acceptance.** The suppression persists across daemon restart;
subsequent `eng_list_findings` calls exclude the suppressed
finding by default and include it with `include_suppressed:
true`; the suppression's `actor_id` and `actor_kind` are recorded
in `audit.jsonl`.

### US-06.01 — Dev: Switch to a stale branch and re-query

**Status:** planned
**Satisfied by:** SOLO-08, SOLO-11

The Dev runs `git checkout <other-branch>`. The post-checkout
hook updates the daemon's notion of the current branch. MCP
queries now scope to that branch's promoted state. If the branch's
promoted graph is older than the working tree, queries return
results plus a `degraded_reasons:
["vector_index_stale_minutes:N"]` notice.

**Acceptance.** Branch switching does not block on a re-scan;
queries return immediately on the existing promoted state; the
daemon kicks off a background re-scan of the changed files and
the staleness notice clears as the queue drains.

### US-07.01 — Dev: Restart the daemon and recover state

**Status:** planned
**Satisfied by:** SOLO-03, SOLO-08

The Dev runs `engram daemon restart` (or the daemon is killed
and respawned). The new daemon process loads promoted state from
SQLite, discards any stale staging, and resumes the post-promotion queue drain
where it left off.

**Acceptance.** The new daemon is ready to serve MCP within the
startup budget (SOLO-13 §3.2 "Cold daemon startup"); pending
post-promotion queue rows from before the restart are drained without
duplication; the user does not need to re-run a cold scan.

## 5. Coverage matrix

| Moment | Dev | Agent |
|---|---|---|
| 01 First install | US-01.01 | — |
| 02 Daily edit loop | US-02.01 | US-02.02 |
| 03 Commit and push | US-03.01 | — |
| 04 Querying the graph | — | US-04.01, US-04.02 |
| 05 Findings | US-05.01, US-05.02 | — |
| 06 Branch switching | US-06.01 | — |
| 07 Daemon restart | US-07.01 | — |

Ten stories. Empty cells are not gaps — they are moments where
the persona has nothing distinct to do (the Agent does not
restart daemons; the Dev does not call MCP tools directly).

## 6. Status

Draft. Stories transition to `shipped` as their satisfying SOLO
section ships and the milestone WBS closes the matching epic.
