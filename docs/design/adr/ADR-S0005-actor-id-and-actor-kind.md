---
id: ADR-S0005
title: actor_id + actor_kind is the entire identity model
status: accepted
date: 2026-05-08
deciders: [whiskeyjimbo]
verified: true
verified_date: "2026-05-16"
---

# ADR-S0005 — actor_id + actor_kind is the entire identity model

## Context

Engram needs to tell humans, agents, and the daemon's own
background work apart on writes, so that:

- The audit log records who did what.
- A human-action gate can require human review before closing a high or
  critical finding.
- Daemon-internal writes (revalidation sweep, embed worker,
  auto-revoke) are distinguishable from agent writes in post-hoc
  review.

That is the entire requirement at V2.0.

The prior design built a `CompositeIdentity{acting_as, on_behalf_of, via}`
3-tuple, an OIDC integration, a `people` plugin slot, a
`compositeidentity` lint analyser that checked every `Save()` carried
the tuple, and a human-action-gate predicate language with seven gates.
None of that is a V2.0 problem. There is one user. The agent runs
as a child of the user's editor. The user's machine is the trust
boundary.

An earlier draft used a `was_human` boolean. That shape reads as
a double negative (`was_human=false` means agent) and cannot
separate agent writes from daemon-internal writes in the audit
log. A three-value enum costs one extra string per row and
covers both.

## Decision

Two columns on every state-changing write (nodes, edges, findings,
suppressions, audit log entries):

- `actor_id TEXT NOT NULL` — a string. Format is one of:
  - `human:<os-username>` — the user who started the daemon.
  - `agent:<name>` — e.g., `agent:claude-code`, set by MCP via the
    `actor_id` header on the connection.
  - `service:veska` — the daemon writing on its own behalf.
- `actor_kind TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system'))`
  — the trust signal. One of:
  - `'human'` — write originated from a CLI invocation by the
    local user.
  - `'agent'` — write originated from an MCP call over the Unix
    socket.
  - `'system'` — write originated from a daemon-internal goroutine
    (revalidation sweep, embed worker auto-attribution,
    auto-revoke).

The daemon derives both columns from the request's origin (which
listener accepted the connection) at write time and stamps them
onto the row. A client connected via the MCP shim cannot
self-declare a different `actor_kind`; the value is set from the
listener, not from the request body.

`actor_id` is a free-form label and may be polluted by an agent
that lies about its name in the MCP handshake. `actor_kind` is
**daemon-set from the accepting listener** — it is the column
the human-action gate reads. Note the limit: a process running as the
same OS user can dial `cli.sock` directly and present as
`'human'`. The OS user is the privilege boundary; SOLO-10 §3.1
makes the framing explicit.

**Single human-action gate, V2.0:**

```
close.finding[severity ∈ {high, critical}] requires actor_kind = 'human'
```

That is the whole gate language. One predicate, one column. If an
agent or a daemon-internal sweep calls `eng_close_finding` on a
high-severity finding, the daemon returns an error with
`reason: "human_required"`. If a future review motivates more
gates, that is an ADR at the time.

The audit log records both columns on every entry, so post-hoc
review can answer "did a human, an agent, or the daemon close
this?" with one column read.

## Consequences

Positive:

- One column-pair, one gate. Trivial to test, trivial to review.
- No identity provider integration in V2.0. The daemon comes up
  with no external dependencies.
- Humans, agents, and daemon-internal writes are distinguishable
  in the audit log without a parser.
- Daemon-internal writes (revalidation sweep) no longer collide
  with genuine agent activity in audit review.
- The human-action gate's predicate is in code, not in a config file or a
  schema. Changing it means landing a Go change with a test, which
  matches the friction expected for a security-relevant change.
- No double-negative readability tax (`actor_kind != 'human'` is
  clearer than `!was_human`, and explicit comparisons read better
  in human-action-gate code).

Negative:

- No notion of "this human signed in 5 minutes ago, has not since
  re-authenticated". Acceptable for a single-user, local-only
  product; the OS session is the auth boundary.
- No delegation chains ("the agent acting on behalf of the user
  via the editor"). If a future workflow needs that, it will need
  a new column or a new ADR. We are not building it unresolvedly.
- Agents that lie about their name in the connection header pollute
  the audit log with a string of their choosing. They cannot,
  however, set `actor_kind = 'human'`.
- One more byte per row vs. an integer boolean. Negligible.

## Alternatives Considered

- **`was_human INTEGER` boolean (earlier draft of this ADR).**
  Loses the agent-vs-system distinction. Reads as a double
  negative. Rejected in favor of the three-value enum.
- **CompositeIdentity 3-tuple (prior ADR-0018).** Three columns
  where two suffice. The `on_behalf_of` and `via` fields had no
  human-action-gate consumer at V2.0; they were sized for a server tier.
  Retracted.
- **OIDC subject claim as actor_id.** Requires an IdP, requires
  refresh, requires a sign-in flow on a single-user laptop. The
  OS user and the MCP connection header are sufficient.
- **Multi-gate trust language (predicate DSL with seven gates).**
  Without consumers for six of the seven, the DSL is overhead. The
  one gate we need fits in one Go function.
- **Human-action gate evaluated client-side by MCP host.** The daemon must
  enforce, not the host; otherwise an agent can bypass by calling
  the daemon directly.
- **Derive `actor_kind` from `actor_id` prefix.** Couples a trust
  signal to a free-form label. An agent could connect with
  `actor_id = "human:alice"` and the prefix would lie. Keeping
  `actor_kind` as a separate, server-set column is the safer
  shape.

## References

- SOLO-10 (identity)
- SOLO-08 §3.1 (`actor_id` and `actor_kind` columns)
