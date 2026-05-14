---
id: SOLO-10
title: "Identity — actor_id and actor_kind"
status: draft
version: 0.2.0
last_reviewed: 2026-05-08
related: [SOLO-01, SOLO-08, SOLO-11, SOLO-15]
---

# SOLO-10 — Identity

The whole identity model is two columns on every state-changing
write:

- `actor_id` — a string naming who did the write.
- `actor_kind` — an enum: `'human'`, `'agent'`, or `'system'`.

That is the whole story. The rest of this file says how those two
values are filled in and what they gate.

## 1. The two values

Every row in `nodes`, `edges`, `findings`, `suppressions`, `tasks`,
and every `audit.jsonl` entry carries `actor_id` and `actor_kind`
(SOLO-08 §3). The application layer refuses to write a row that
lacks them.

### 1.1 `actor_id`

A free-form string. It is a label, not a trust signal — an agent
that lies in its MCP handshake can pollute it. The human-action gate
reads `actor_kind`, never `actor_id`. By convention:

| Source | Value |
|---|---|
| The user who started the daemon | `human:<$USER>` (e.g. `human:jeff`) |
| An agent connected via MCP | `agent:<name>` where `<name>` comes from the agent's MCP handshake (e.g. `agent:claude-code`) |
| The daemon writing on its own behalf (background sweeps, auto-revoke) | `service:engram` |

The string is opaque to the schema. The daemon does not parse it,
does not split on `:`, does not look up a directory record.

### 1.2 `actor_kind`

A three-value enum. **Daemon-set from the accepting listener,
not from the request body.** A normal client (one that uses the
shipped binaries) cannot supply or override it.

| Origin | `actor_kind` |
|---|---|
| Connection accepted on `~/.engram/cli.sock` | `'human'` |
| Connection accepted on `~/.engram/mcp.sock` | `'agent'` |
| Daemon background goroutine (revalidation sweep, embedder, auto-revoke) | `'system'` |

The daemon binds two Unix sockets (SOLO-03 §3). The accepting
listener — not anything in the request — determines `actor_kind`,
so the value is set before the request body is even read. A
well-behaved MCP client cannot present itself as a human by
lying in a header because there is no header to lie in:
`actor_kind` is a property of the file the client connected to.

The CLI binary `engram` connects only to `cli.sock`; the stdio
shim `engram-mcp` connects only to `mcp.sock`. A user who
deliberately points the CLI at `mcp.sock` (`engram --socket=...`)
is declaring themselves an agent and the human-action gate behaves
accordingly — that is the intended behavior for scripted use.

Why two sockets at all: the gate (§3) refuses high-severity
writes from `actor_kind != 'human'`. If a single socket carried
both kinds, the daemon would have to read the client's
self-declaration before deciding whether to trust the call — so
a benign editor extension could *accidentally* close a critical
finding by claiming the human's role in its handshake. Two
sockets remove the self-declaration field entirely; the gate
reads from a property the typical client cannot influence.

What two sockets do **not** do: stop a process running as the
same OS user from dialling `cli.sock` directly. Any code that
already has user-level execution on the machine can connect to
either socket. The OS user is the security boundary; the socket
split is a clean separator for *typical* clients (editor MCP
shim vs. terminal CLI) so the daemon and the audit log reflect
the difference. §3 makes this explicit.

Even when a human's editor talks to the daemon, the editor is
an agent: it could be scripted, it could be the AI in a vibe-
coding loop, it could be either. We do not try to guess. If the
human wants their action recorded as human, they use the CLI.

Daemon-internal goroutines stamp `actor_kind = 'system'` on the
rows they write — this separates them from genuine agent activity
in audit review. The revalidation sweep, the auto-link re-scorer,
the soft-suppression revoker, and the embed worker's
auto-attribution writes all use `'system'`.

**The review-pipeline exception.** The review pipeline (SOLO-11
§3) generates findings whose *content* came from an LLM and whose
*scheduling* is daemon-driven. Those findings carry
`actor_kind = 'system'` (because the daemon goroutine wrote the
row) **and** `actor_id = "agent:<llm-generator-name>"` (so audit
review can see *which* model produced the rationale). This split
is deliberate: `actor_kind` answers "did a human, an agent, or
the daemon write this row?" — the answer is "the daemon"; the
review goroutine is one of the system writers in §1.2's table.
`actor_id` answers "what produced the *content*?" — the answer
is the LLM. The human-action gate (§3) reads `actor_kind` and refuses
the row's high-severity *closure* without a human, regardless
of which LLM authored the rationale.


#### 1.2.1 Embed worker — what kind?

The embed worker writes `node_embedding_refs` rows after Ollama
returns a vector for a promoted node. The worker writes
`actor_kind = 'system'` on those refs because the embedding step
is daemon-driven and time-shifted from the original promotion. The
promoted `nodes` row carries the original `actor_kind` of whoever
authored the content; the embedding ref is daemon work. Audit
review keeps both.

## 2. How `actor_id` is resolved

### 2.1 CLI

CLI invocations connect to `~/.engram/cli.sock`. The wire frame
on the first message of the connection is:

```jsonc
{
  "jsonrpc": "2.0",
  "id":      1,
  "method":  "initialize",
  "params": {
    "client": "engram-cli",
    "user":   "<value of $USER from the CLI process env>",
    "engram_version": "<x.y.z>"
  }
}
```

The daemon takes `actor_id = "human:" + params.user`. The `user`
field is supplied by the CLI binary at connection time from
`$USER`; the daemon does not consult its own environment. If
`$USER` is unset in the CLI's env (rare on Linux, possible in a
headless container), the CLI sends `user: "unknown"` and the
daemon stamps `actor_id = "human:unknown"` with a logged warning.
There is no login, no OS keyring, no `engram login`.

`actor_kind` for connections accepted on `cli.sock` is always
`'human'` regardless of the `params.user` value — the listener
is the gate substrate (§1.2), not the request body.

### 2.2 MCP

When an MCP client connects to `~/.engram/mcp.sock`, the first
frame on the connection is an `InitializeParams` payload. The
daemon reads `clientInfo.name` from that payload and uses it as
`actor_id = "agent:" + name`. Examples: `agent:claude-code`,
`agent:cursor`, `agent:zed`.

If the client does not provide a name, the daemon assigns
`actor_id = "agent:anon-" + <random 8 chars>` for the duration of
the connection and logs a warning. The connection still works;
attribution is just less useful.

`actor_kind` for connections on `mcp.sock` is always `'agent'`.
The handshake's contents do not influence the kind — only the
`actor_id` label.

### 2.3 Daemon-internal

Background goroutines write under `actor_id = "service:engram"`
with `actor_kind = 'system'`. The revalidation sweep, the
auto-link re-scorer, the soft-suppression revoker, the embedder
worker — all of them.

## 3. The single human-action gate

There is exactly one gate:

> **Closing a finding with `severity >= high` requires
> `actor_kind = 'human'`.**

The `Severity` enum is ordered (`info < low < medium < high <
critical`; SOLO-04 §8.1). The gate uses the ordering, not a set
enumeration, so a future severity above `high` is gated
automatically.

Written in pseudocode:

```go
func (s *Service) CloseFinding(ctx context.Context, id FindingID, reason string) error {
    f := s.repo.Get(id)
    if f.Severity >= SeverityHigh {
        if ctx.ActorKind() != ActorKindHuman {
            return ErrHumanActionRequired{Gate: "close.finding.high", Reason: "requires actor_kind=human"}
        }
    }
    return s.repo.Close(id, reason, ctx.ActorID(), ctx.ActorKind())
}
```

That is it. There is no policy file. There is no predicate
language. There is no role hierarchy. There is no `acting_as` /
`on_behalf_of` / `via` triple. There is no `requires: committer`
or `requires: admin`. There is one rule, hardcoded, in one place
in the code.

### 3.1 What the gate is, and what it isn't

The gate is a **UX and audit guardrail**, not a security
boundary. Be precise about both halves:

**What it does.**

- Stops the editor's MCP path from silently dismissing high or
  critical findings while the human is heads-down. The agent has
  to surface the close to the human and let them type it; that
  is enough friction to ensure the human looked.
- Makes the audit log say *which* path the close came through.
  A high-severity close is always attributable to a CLI
  invocation, not an MCP one — useful when reading
  `audit.jsonl` later.
- Catches accidental MCP-side closures from a benign editor
  extension or a scripted agent that didn't mean to touch the
  finding. This is the common failure mode on a laptop.
- Forces daemon-internal sweeps (`actor_kind = 'system'`) to
  *propose* closes rather than perform them. Revalidation never
  silently retires a critical finding (SOLO-11 §6.1).

**What it does not do.**

- It does not stop a process running as the same OS user from
  closing high-severity findings on its own. That process can
  connect to `cli.sock` and present as `actor_kind = 'human'`,
  or it can shell out to `engram finding close ...` directly.
  The OS user is the only privilege boundary on the machine;
  Engram does not invent another one.
- It does not authenticate the human. A second person with shell
  access on the same account is indistinguishable from the user
  who started the daemon — by design, given the single-user
  product (SOLO-01 §1).
- It does not bind the close to the editor's consent flow. The
  editor's "are you sure?" prompt happens above the daemon; if
  the editor is compromised or scripted, the gate's only
  remaining job is to record that the close went through the
  CLI path.

For a one-user laptop product, this scope fits the failure
mode that actually occurs: most
"hey wait, the agent just closed a critical finding" failures
are accidents, not adversarial actions. We design for the
common failure mode and rely on the OS for the rest.

A real role hierarchy, a token, an editor-to-CLI approval handoff
that doesn't require pasting into a terminal — those return only
if the product gains a second user.

### 3.2 Why this rule (and not others)

- High and critical findings (vulns, secret leaks, contract
  breaks) are the ones whose silent dismissal causes real
  damage.
- The agent should not close them on its own — even when it
  thinks the finding is a false positive, the human types the
  close command. The §3.3 handoff makes that one paste away.
- A daemon-internal sweep cannot close them either —
  `actor_kind = 'system'` fails the same gate.
- Everything else — opening findings, suppressing low-severity
  findings, writing nodes/edges/tasks — runs without a gate.

### 3.3 Editor ↔ CLI handoff

The gate creates a friction point: an agent in the editor cannot
close a high or critical finding, even when the human is sitting
right there agreeing with the agent's verdict. The handoff design
keeps the terminal as the only attestation path while making the
trip out of the editor cheap.

When `ErrHumanActionRequired` is returned, the daemon populates the
error's `data` payload with a ready-to-paste CLI invocation:

```jsonc
{
  "error": {
    "code": -32001,
    "message": "human-action gate refused: close.finding.high requires actor_kind=human",
    "data": {
      "engram_code": "ErrHumanActionRequired",
      "context": {
        "gate":        "close.finding.high",
        "finding_id":  "f_01HK...",
        "severity":    "critical",
        "cli_command": "engram finding close f_01HK... --reason \"false positive: stub credential\""
      }
    }
  }
}
```

The editor renders `cli_command` as a copyable code block with a
"copy" affordance. The human pastes into a terminal, hits enter,
returns to the editor. The agent's next read sees the finding
closed.

`cli_command` reuses the agent's `reason` string verbatim — the
agent's argument to `eng_close_finding` is preserved through the
terminal trip so the human is not retyping rationale. The CLI
invocation is the human-action attestation; nothing about this scheme weakens
the gate. The audit log records two lines: the agent's refused
attempt (`actor_kind: "agent"`, `result: "refused: ..."`) and the
human's successful close (`actor_kind: "human"`).

What this is **not**: there is no token, no approval file, no
short-lived credential the editor can pass back to the daemon.
Adding any of those is a future ADR. The
terminal is the attestation path; per §3.1 it is not a security
boundary against same-user code. The editor's role is to make
reaching the terminal one paste away.

**Batch close.** The CLI accepts multiple finding IDs in a
single invocation:

```
engram finding close f_01HK... f_01HL... f_01HM... --reason "..."
```

The single `--reason` applies to every ID. The daemon writes
one transaction per finding (each goes through the gate
independently); the audit log records one `human:<user>` line
per close, all with identical timestamps modulo microseconds.
This is a friction reducer for vuln-heavy review cycles where
the human has already decided "yes, all of these are false
positives." It does **not** weaken the gate — every close still
arrives via `cli.sock` and is attributable to the human. The
editor's `cli_command` payload renders the multi-ID form when
the agent's request listed more than one finding to close.

## 4. The audit log

`audit.jsonl` is the daemon's append-only attribution record. The
application layer writes one JSON line synchronously, before the
operation returns, in three cases:

1. **Every state-changing write.** All MCP tools that mutate the
   graph, findings, suppressions, or tasks.
2. **Every refused write.** Human-action-gate refusals and any other
   server-side rejection — the *attempt* is recorded with
   `result` describing the refusal.
3. **Every read whose answer used cross-repo resolver work.** The
   resolver materialises synthetic edges (SOLO-04 §5.4, SOLO-11
   §9) and captures a per-target `as_of` tuple at query start;
   without recording both, post-hoc audit cannot reproduce why
   an agent saw a given edge. The audit append is synchronous on
   the read path and is counted explicitly in the cross-repo
   read budget (SOLO-13 §3.4) — typical-read p95 budgets assume
   no audit append (same-repo reads); cross-repo reads carry the
   audit-append cost in their separate budget. Same-repo reads
   do not write audit.

Shape (illustrative; the canonical contract — fields, types,
versioning rule — lives in SOLO-08 §3.5):

```json
{
  "v": 1,
  "ts": "2026-05-08T15:42:31.123456Z",
  "actor_id": "agent:claude-code",
  "actor_kind": "agent",
  "tool": "eng_close_finding",
  "args": {"finding_id": "f_01HK...", "reason": "false positive"},
  "result": "refused: human_action close.finding.high requires actor_kind=human"
}
```

Refusals and successes are logged. Every line carries `v`,
`actor_id`, and `actor_kind`. Rotate at 100MB; keep five. This
section names the trigger; SOLO-08 §3.5 owns the schema and
stability rules.

**Trust scope.** `actor_kind` is derived from the connecting
socket (`cli.sock` ⇒ human; `mcp.sock` ⇒ agent — §3.1) and
nothing else. Any same-user process can dial `cli.sock`, so
`actor_kind=human` is **attribution against well-behaved
same-user processes**, not authentication. The audit log is
trustworthy *to the extent the user trusts every process running
under their UID*. This is enough for the design's stated
purpose (post-hoc reconstruction of which path a write came
through, plus a UX guardrail against reflexive agent closes —
§3) and is not enough for a multi-tenant or hostile-process
threat model. Compromised same-user code can append
`actor_kind=human` lines indistinguishable from a human's. We
do not promise otherwise. Workspaces hosting untrusted code
should rely on OS-level sandboxing, not on `audit.jsonl`'s
`actor_kind` field, for accountability.

## 5. What's NOT here

This list is normative. Adding any row is a redesign, not a
feature. The "what would change" column names the prerequisite
that would need to be true; none of these prerequisites are on
the V2 roadmap.

| Denial | What would change to add it |
|---|---|
| **No OIDC.** No bearer tokens, no JWTs, no token refresh. | A second user, or remote access; both require a server tier. |
| **No SCIM.** No directory sync. | An organisational deployment — not a one-laptop product. |
| **No SAML.** No identity providers of any kind. | Same as SCIM. |
| **No `people` slot.** No plugin interface for an identity directory. The `actor_id` string is the identity. | A second user with a name the daemon doesn't already know from `$USER`. |
| **No composite identity.** No `acting_as` / `on_behalf_of` / `via` triple. | A delegation model where one actor authorises another to write — not the case here, since the human and the agent both write directly. |
| **No predicate language.** No CEL, no Rego, no DSL. The gate is one `if` statement. | More than one gate, or a gate that depends on per-finding metadata beyond severity. |
| **No group-to-role mapping.** No groups. No roles. No `requires: committer`. | A multi-user product with privilege tiers. |
| **No IDP integration.** No Okta, no Google Workspace, no AD, no GitHub Org. | A server tier with team-managed access. |
| **No federation.** No `urn:engram:actor:...`, no DIDs, no ActivityPub. | A cross-machine product where multiple Engram instances exchange data. |
| **No identity merge.** No aliasing across emails, no email-change handling. | A history that needs to track a single person across renames — not relevant when `actor_id` is a label. |
| **No personal-data scrub.** Right-to-be-forgotten is not a workflow. | A regulatory obligation that applies to a multi-user audit log. |
| **No fourth `actor_kind` value.** `'human'`, `'agent'`, `'system'` is the closed set. | A new origin category that doesn't fit any of the three (e.g., a federated peer). |

If a server tier is ever built, every row above is ADR work and
lives under `docsv2solo/design/deferred/`. None of it is in this
doc tree.

## 6. The agent-delegation question

Q: how does the system know that an agent action is "really"
the human's action?

A: it doesn't, and it doesn't need to.

- The human runs the daemon. The daemon's process owner is the
  human's OS user. Anything else with that user's privileges is
  outside Engram's threat model (§3.1).
- The human approves agent actions through the editor's MCP
  consent flow. That happens above the daemon, in the editor.
- When an agent edit reaches the daemon, the daemon records
  `actor_id = "agent:<name>"` and `actor_kind = 'agent'` — based
  on which socket accepted the connection (§1.2), not on
  anything the agent claimed. A *typical* MCP client cannot
  forge that label; a hostile process running as the same user
  can dial `cli.sock` directly and label itself however it
  wants. The OS user is the only privilege boundary.
- If the human wants the audit log to say "I did this myself",
  they type the command in the CLI. That is the canonical way
  to set `actor_kind = 'human'`, and it is the path the human-action
  gate (§3) is designed around.

This collapses the agent-delegation problem into "did the
command come through the CLI listener, the MCP listener, or a
daemon goroutine" — a distinction useful for the audit trail and
for catching accidental MCP-side closures. It is sufficient for
one user on one machine; it is not a substitute for OS-level
isolation in any other setting.

## 7. Diagnostics

`engram doctor identity` prints:

- The current process owner (`$USER`).
- The resolved CLI `actor_id`.
- The list of currently-connected MCP clients with their
  declared `actor_id`s.
- The path to the audit log and its current size.
- Any refused writes in the last 24 hours, with their gate name
  and reason.

That is the entire identity-diagnostics surface. There is no
"explain why this gate failed" because there is one gate and its
explanation is "this finding has `severity >= high` and
`actor_kind` was not `'human'`".
