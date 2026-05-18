---
id: SOLO-09
title: "MCP Surface — Tools, Naming, Transport"
status: draft
version: 0.1.0
last_reviewed: 2026-05-17
verified: true
verified_date: "2026-05-18"
related: [SOLO-01, SOLO-03, SOLO-04, SOLO-08, SOLO-11, SOLO-12, SOLO-15]
---

# SOLO-09 — MCP Surface

The MCP surface is how the editor and the AI agent talk to the
daemon. 33 registered tools (as of M5), one transport, a flat
naming scheme, and a small output contract. This file is the
whole surface; there are no sub-files.

> **Verification note (2026-05-18):** All 33 tools listed in §3
> below are registered in `internal/infrastructure/mcp` as of M5
> close. The record/repo tools — `eng_get_finding`,
> `eng_get_suppression`, `eng_close_suppression`, `eng_add_repo`,
> `eng_remove_repo` — were registered by `solov2-nz2.7`;
> `eng_find_changed_symbols` was added by `solov2-4j5`.

## 1. Transport

One transport, two listeners: Unix domain sockets at
`~/.veska/cli.sock` and `~/.veska/mcp.sock`. Both are created
by the daemon at startup with mode `0600` and ownership matching
the user that started the daemon. JSON-RPC 2.0 over each. No
TCP, no TLS, no auth — file-system permissions are the gate. If
either socket is missing or unreachable, the caller sees a clean
connection error.

The two listeners speak the same wire protocol; they differ
only in which `actor_kind` the daemon stamps on requests they
accept (SOLO-10 §1.2). The CLI connects only to `cli.sock`; the
`veska-mcp` stdio shim connects only to `mcp.sock`. The shim
is a thin process: editors that speak MCP-over-stdio (Cursor,
Claude Code, Codex, Zed) point at it; it proxies to
`mcp.sock`. The daemon does not speak stdio directly.

There is no third transport. If a future ADR adds one, it is an
ADR; we do not pre-shape the surface for it.

### 1.3 Two surfaces on one wire — MCP tools vs. control RPC

Both sockets carry JSON-RPC 2.0 frames. Each frame's `method`
string belongs to one of two namespaces; the single driving
port `RPCHandler` (SOLO-07 §4.3a) dispatches to the matching
internal sub-router:

| Namespace | Sub-router | Examples | Surface |
|---|---|---|---|
| `eng_<verb>_<object>` | MCP sub-router | `eng_find_symbol`, `eng_close_finding`, `eng_add_repo` | This file (§3) |
| `daemon.<verb>` | Control sub-router | `daemon.promotion`, `daemon.backup_create`, `daemon.embedder_swap`, `daemon.doctor`, `daemon.supervise_heartbeat`, `daemon.post_checkout` | §1.3a below |

The split is *what shape fits the verb*, not *which client sent
the verb*. Both sockets accept both namespaces; a control verb
arriving on `mcp.sock` is dispatched the same way as one arriving
on `cli.sock`. The actor-kind gate (SOLO-10 §3) still applies:
a high-severity close arriving from `mcp.sock` is refused
regardless of which router handles it.

#### 1.3a Control RPC inventory

| Verb | Caller | Purpose |
|---|---|---|
| `daemon.promotion` | `veska hook-runner post-commit` | Atomically promote staging → SQLite for the repo at the named path. |
| `daemon.post_checkout` | `veska hook-runner post-checkout` | Branch-switch quiescence (SOLO-11 §1.3). |
| `daemon.backup_create` | `veska backup create` | Run `VACUUM INTO` + tarball; verify. |
| `daemon.backup_verify` | `veska backup verify <path>` | Run integrity + foreign-key + JSONL well-formedness checks. |
| `daemon.embedder_swap` | `veska embedder swap <model>` | The multi-step procedure in SOLO-03 §3.2. |
| `daemon.embedder_current` | `veska embedder current` | Read `database_meta.embedder_*`. |
| `daemon.doctor` | `veska doctor [--json]` and every `veska doctor <section>` | Run the doctor section(s) and return the §2.1 envelope. |
| `daemon.bundle` | `veska bundle` | Build the `veska-doctor-bundle-*.tgz`. |
| `daemon.gc_branches` | `veska gc --branches` | Run the branch-GC sweep manually. |
| `daemon.supervise_heartbeat` | `veska supervise` | Liveness ping for the built-in supervisor. |
| `daemon.upgrade_stage` | `veska upgrade <path>` | Stage `*.next` binaries; the running daemon is unaffected until restart. |

These verbs do not fit `eng_<verb>_<object>` (they are
lifecycle, not graph/finding/task operations) and forcing them
into the tool shape would distort the MCP surface. Both surfaces
are wire-clean per §6 and `wireclean`.

A read-only CLI verb that *does* fit the tool shape (e.g.,
`veska find-symbol`, `veska blast-radius`) reuses the matching
`eng_*` tool over the same socket — no duplicate handler. CLI
verbs in the design tree are CLI-side verbs; their underlying
RPC is whichever namespace fits.

## 2. Naming

Every tool name has the form:

```
eng_<verb>_<object>
```

- `eng_` is the vendor prefix. It is fixed.
- `<verb>` is drawn from the closed set in ADR-S0008. Adding a verb
  requires an ADR amendment to that file.
- `<object>` is the singular thing the tool acts on
  (`symbol`, `node`, `task`, `finding`, etc.). No domain or port
  prefix; the verb plus the object is enough.

Anything that does not fit one of the verbs in ADR-S0008 gets
renamed until it does, or it is not a tool — it is a CLI command
or a config flag.

## 3. Tool inventory

33 registered tools, flat table.
The "Staging" column is `yes` if the tool
reads through the staging overlay (sees unpromoted edits) or `no`
if it reads promoted state only (lags an in-flight save by the
time to next promotion). On a per-response basis the answer is
echoed in the response envelope as `included_staging` (§4.4).
W = write; R = read.

### 3.1 Graph

| Tool | Purpose | Staging | W/R |
|---|---|---|---|
| `eng_find_symbol` | Resolve symbols by name + optional kind/path filters. Accepts `repo` arg (default: current; `"*"` = all indexed). | yes | R |
| `eng_get_node` | Fetch one node by `NodeID`. | yes | R |
| `eng_get_call_chain` | Bounded BFS over `CALLS` edges from a node. Crosses repos when an edge does. | yes | R |
| `eng_get_file_nodes` | Nodes contained in a path. | yes | R |
| `eng_search_semantic` | Vector search over node embeddings. Cross-repo via `repo` arg. | no | R |
| `eng_search_similar` | Top-k similar nodes to a given node by embedding. | no | R |
| `eng_get_blast_radius` | Blast radius for a node (promoted graph). Cross-repo via `repo` arg — your service signature change → who in your other indexed repos calls it. | no | R |
| `eng_get_dirty_blast_radius` | Blast radius including staging-area edits. | yes | R |
| `eng_get_diff_blast_radius` | Blast radius union over a working-tree diff (staging vs promoted `HEAD`). | yes | R |
| `eng_find_changed_symbols` | Symbols added/removed/modified between two git refs (`ref_a`, `ref_b`). Parses the changed files at each ref on demand — no history substrate; never reads the promoted graph. Single-repo. | no | R |

### 3.2 Task

| Tool | Purpose | Staging | W/R |
|---|---|---|---|
| `eng_set_active_task` | Pin the active task for the session. | yes | W |
| `eng_get_active_task` | Return the current active task. | yes | R |
| `eng_get_task_history` | Nodes touched while this task was active. | yes | R |
| `eng_find_todos` | TODOs in the active task's blast radius. | yes | R |
| `eng_get_context_pack` | Bounded, token-budgeted context pack for a `{symbol}` or `{task_id}` (exactly one required). Bundle: relevant nodes, recent commits, open findings, tasks. | yes | R |

### 3.3 Finding & suppression

| Tool | Purpose | Staging | W/R |
|---|---|---|---|
| `eng_list_findings` | List open findings, filterable by source layer / severity / scope. | yes | R |
| `eng_get_finding` | One finding by id. | yes | R |
| `eng_close_finding` | Close a finding with a reason. | yes | W |
| `eng_reopen_finding` | Reverse a close. Carries the original close-reason in history. | yes | W |
| `eng_suppress_finding` | Apply a suppression scoped to symbol / file / repo / finding-id. Optional `expires_at`. Optional `branch` (NULL ⇒ all branches; SOLO-04 §8.2). The agent must pass `branch` explicitly — there is no implicit default to avoid silent cross-branch silencing. | yes | W |
| `eng_list_suppressions` | List active suppressions in the current scope. | yes | R |
| `eng_get_suppression` | One suppression by id. | yes | R |
| `eng_close_suppression` | Terminate an active suppression now (sets `expires_at = now`). | yes | W |

### 3.4 Repos & ownership

| Tool | Purpose | Staging | W/R |
|---|---|---|---|
| `eng_get_current_repo` | Resolve the active repo from CWD. | yes | R |
| `eng_get_repo` | One repo's detail by id (root path, branch, last-promoted SHA, embed-queue depth). | yes | R |
| `eng_list_repos` | Enumerate every repo the daemon has indexed. | yes | R |
| `eng_add_repo` | Register a new repo path; daemon kicks off cold-scan in the background. | yes | W |
| `eng_remove_repo` | Unregister a repo and drop its rows. | yes | W |
| `eng_find_owner` | Resolve owners for a file or symbol. Returns both CODEOWNERS-declared owners and `git blame`-derived contributors. See §5.1. | yes | R |

### 3.5 Wiki

| Tool | Purpose | Staging | W/R |
|---|---|---|---|
| `eng_get_hot_zone` | The graph's high-centrality / high-churn page (SOLO-12). | no | R |
| `eng_get_entry_points` | The starter onboarding page (SOLO-12). | no | R |

### 3.6 Status & config

| Tool | Purpose | Staging | W/R |
|---|---|---|---|
| `eng_get_status` | Daemon health, child-proc state (Ollama up?), schema version, queue depths. The "is the daemon ready to serve this call?" check. | yes | R |
| `eng_get_config` | The daemon's effective config (defaults merged with `~/.veska/config.toml`). Includes every configured outbound destination (LLM provider, OTLP endpoint, vuln-source URLs). **Secrets are redacted** (`***`). The `veska doctor config --show-secrets` CLI is the only path that returns them in clear. | yes | R |

`doctor` is a CLI noun, not an MCP one — `veska doctor` (and its
subcommands) is the operator surface; the two tools above are the
agent's window into the same data. Diagnostics that *fix* things
(repair, gc, embedder swap) are CLI-only on purpose.

Total: 33 registered tools.
`eng_context_pack` from SOLO-12 is the same binding as
`eng_get_context_pack` above; the wiki section references it by
purpose, this section names it.

The time-travel tool `eng_get_node_as_of` is not present. The
substrate stores only the latest promoted state per branch — there
is no per-commit history of node bodies — so a point-in-time node
query against an arbitrary SHA would require a stored-history
substrate that does not exist. It lands when (and if) one does,
behind an ADR. `eng_find_changed_symbols` (§3.1) sidesteps this:
rather than reading stored history it parses the changed files at
two refs on demand and diffs the symbol sets, so it needs no
substrate.

### 3.7 The cross-repo argument

Several tools accept a `repo` arg using this convention:

| Value | Meaning |
|---|---|
| omitted | The current repo (resolved from CWD as for `eng_get_current_repo`). |
| `"<repo_id>"` | That specific indexed repo. |
| `["<id1>", "<id2>"]` | A subset of indexed repos. |
| `"*"` | Every repo the daemon has indexed. |

Tools that accept `repo`: `eng_find_symbol`, `eng_search_semantic`,
`eng_search_similar`, `eng_get_blast_radius`, `eng_get_call_chain`,
`eng_get_diff_blast_radius`, `eng_list_findings`,
`eng_list_suppressions`, `eng_find_owner`.

Cross-repo edges are **synthesized at query time** by the resolver
chain (SOLO-11 §9), not stored (SOLO-04 §5.4, SOLO-08 §6.1). When a
traversal tool emits an edge whose target is in another indexed
repo, the response tags it:

```jsonc
{
  "src":            "<NodeID in repo A>",
  "tgt":            "<NodeID in repo B>",
  "kind":           "CALLS",
  "cross_repo":     true,
  "target_repo_id": "<repo_id of B>",
  "target_branch":  "<B's active_branch at query time>"
}
```

If you haven't indexed B, the source-side promotion recorded a
`CrossRepoStub` (SOLO-04 §5.2.1) in `cross_repo_edge_stubs`,
not an `Edge`. The traversal projects the stub into the response
with `confidence: "unresolved"`, `tgt: null`, and
`cross_repo_target: {module_path, symbol_path, language}` so
the agent can see what import path failed to resolve. The
synthetic-edge tag is not set (the target hasn't been indexed
yet, so there is nothing to synthesize). When the target repo
is later registered and promotions the symbol, the next query
projects the same stub as a fully-tagged synthetic edge — no
storage rewrite, just a different read-time view.

**One-hop default.** Traversal tools (`eng_get_call_chain`,
`eng_get_blast_radius`, `eng_get_diff_blast_radius`) stop at the
cross-repo node. Pass `expand_cross_repo: true` to continue
walking into the target repo. The default keeps blast-radius
bounded across a working set of N repos.

**Per-query snapshot, bounded staleness.** Cross-repo answers
reflect each participating target's promoted state *as captured at
query start* (SOLO-04 §5.4 invariant 2). The response envelope
carries `as_of: [{repo_id, branch, promoted_sha}, ...]` listing
every repo the resolver touched and the `promoted_sha` it read
as-of. Two queries seconds apart can return different cross-repo
edges if a target promoted in between; `as_of` makes the
difference auditable. This is documented behaviour, not a bug;
SOLO-04 §5.4 explains the trade.

## 4. Output contract

### 4.1 Default node projection

Tools that return nodes return the **summary projection** by
default:

```jsonc
{
  "node_id":   "<NodeID>",
  "name":      "<symbol or section name>",
  "signature": "<one-line signature, if applicable>",
  "summary":   "<heuristic, ≤ 280 chars>"
}
```

`raw_content`, `lines`, and `attributes` are opt-in via per-tool
flags (`include_body: true`, `include_lines: true`,
`include_attributes: true`). Traversal tools accept
`expand_depth: N` (default 1).

### 4.2 Unresolved edges

Traversal tools that walk edges (`eng_get_call_chain`,
`eng_get_blast_radius`, `eng_get_dirty_blast_radius`,
`eng_get_diff_blast_radius`) **exclude edges with `confidence ==
Unresolved` by default**. Pass `include_unresolved: true` to
include them. The promoted graph keeps unresolved edges; the
default is a read-side filter.

### 4.2a Active-task scoping

Every tool description (§6) declares `Active task: required |
optional | not-used`. Tools with `optional` use the active task as
a default scope when one is set, and ignore it otherwise. Tools
with `required` return `ErrInvalidArgs` if no active task is set.
Tools with `not-used` ignore it.

Currently `required`: `eng_find_todos`, `eng_get_task_history`.
`eng_get_context_pack` takes an explicit `{symbol | task_id}`
argument rather than reading the active-task scope, so it is
`not-used` for active-task scoping. Everything else is `optional`
or `not-used`.

### 4.3 Token budget

| Knob | Default | Ceiling |
|---|---|---|
| `MAX_TOKENS_DEFAULT` | 8 000 | — |
| `MAX_TOKENS_CEILING` | — | 24 000 |

A per-response token estimate is computed as the response is
built using the configured `TokenEstimator` port (SOLO-05 §2.11).
Default impl is `chars/4` — a deliberately approximate
heuristic; documented as such so cap behaviour is interpretable
against the estimator that produced it. The estimator's
`ModelHint()` is recorded in the audit line for any truncated
response. When the estimate hits `MAX_TOKENS_DEFAULT`, the
response is truncated and
`degraded_reasons: ["response_truncated:tokens"]` is set.
Callers may request larger responses up to `MAX_TOKENS_CEILING`
via `max_tokens: <int>`.

Truncation rules:

- Lists return the highest-ranked items per the tool's ordering;
  envelope carries `truncated_at: N`.
- Traversals return the closest hops first; envelope carries
  `truncated_at_depth: D`.
- Single-row tools (`eng_get_node`) never truncate the row; if
  the body alone exceeds the ceiling, the body is omitted and a
  warning points the caller to fetch with `include_body: true`.

### 4.4 Staging visibility

Responses that read from staging carry a single boolean,
`included_staging: true`. Responses that read promoted state only
omit the field (or set `false`). That is the entire freshness
surface: one process, one daemon, one user — there are not
multiple coherence levels worth distinguishing on the wire.

```jsonc
{
  "result": [...],
  "included_staging": true        // present only when relevant
}
```

A tool that *can* read staging may return `included_staging:
false` for an individual response (e.g. staging-overlay read
failed; result reflects promoted state only). When that happens
the response also sets `degraded_reasons: ["staging_unavailable"]`
so callers see *why* the staging view is missing — the boolean
alone would conflate "no staging activity for this query" with
"staging was unreachable."

There is no closed enum, no `freshnessUnset` zero-value
rejection, no `RegisterTool` panic for a missing freshness
field. Tool registration validates name, verb, and write
semantics (ADR-S0008); whether a tool reads staging is a
property of its handler, not a declaration on its spec.

`RegisterTool` validates name and verb against the closed set
(ADR-S0010); whether a tool reads staging is a handler property,
not a `ToolSpec` field. No custom Go analyser; the type system
and the registration constructor do the work.

### 4.5 Degraded reasons

When a tool succeeds with caveats, the envelope includes:

```jsonc
{
  "result": [...],
  "degraded": true,
  "degraded_reasons": [
    {"code": "vector_index_stale", "details": {"minutes": 8}},
    {"code": "pipeline_queue_depth", "details": {"depth": 142}},
    {"code": "memory_pressure"},
    {"code": "response_truncated", "details": {"limit": "tokens"}}
  ]
}
```

`degraded_reasons` is an **open object set**. Each entry has a
required `code` (snake_case, drawn from the table below) and an
optional `details` object whose schema is per-code. The protocol
contract: "any non-empty list means the answer is partial, and
each entry is informative to the operator." Common codes:

| Code | `details` shape | Meaning |
|---|---|---|
| `vector_index_stale` | `{minutes: int}` | Embedding queue lag. |
| `pipeline_queue_depth` | `{depth: int}` | post-promotion queue queue depth. |
| `memory_pressure` | — | Daemon is over its memory soft-cap. |
| `response_truncated` | `{limit: "tokens" \| "rows"}` | Output hit a budget. |
| `staging_unavailable` | — | Staging-overlay read failed; promoted-only result. |
| `staging_reparsing` | `{path: string}` | Large-file fallback active for `path` (SOLO-11 §1, SOLO-13 §3.1b). |
| `embedding_pending` | `{node_count: int}` (optional) | Affected nodes' embeddings are still in the post-promotion queue. |
| `embedding_failed` | `{node_count: int}` (optional) | Affected nodes' embeddings exhausted retries. User retries via `veska doctor post-promotion-queue retry --kind=embed`. (SOLO-08 §3.4, ADR-S0004.) |
| `embedder_offline_lexical_fallback` | — | `eng_search_semantic` returned BM25 from FTS5 (SOLO-08 §3.3) instead of vector recall. The agent should not silently switch reasoning modes. |
| `post_promotion_queue_deferred` | `{work_kind: string, count: int}` | Rows in `state='deferred'` because queue depth was at high-water at promotion time (SOLO-08 §3.4). |
| `startup_resync` | `{repos_pending: int}` | Daemon is replaying `git log <last_promoted_sha>..HEAD` (SOLO-03 §5.7). |
| `wake_reconciling` | — | Daemon detected a suspend/wake gap and is sweeping repos (SOLO-03 §5.2). |
| `embedder_swapping` | — | Daemon is mid-`veska embedder swap` (SOLO-03 §3.2). |
| `vec0_ceiling_warn` | `{headroom_ratio: float}` | Approaching the vec0 substrate ceiling (SOLO-13 §3.3.1). |
| `vec0_ceiling_exceeded` | `{headroom_ratio: float}` | Past the ceiling; `semantic_search` p95 budget likely missed. |

`degraded: true` is required whenever any entry is present. New
codes must keep the object shape; introducing one does not
require an ADR. Add it next to the relevant code path; surface
it via `veska doctor` if the operator should act. Clients
written against an older code vocabulary MUST tolerate unknown
codes (forward compat).

### 4.6 Errors

JSON-RPC error format with structured `data`. **The full
catalogue lives in SOLO-16**; this section names the envelope
shape only:

```jsonc
{
  "error": {
    "code": -32001,
    "message": "human-readable summary",
    "data": {
      "veska_code": "<symbolic code>",   // SOLO-16 is the catalogue
      "context":     { /* code-specific */ }
    }
  }
}
```

`veska_code` is an open vocabulary — new codes can land next
to new code paths without ceremony — but every code MUST appear
in SOLO-16 before it ships. Tooling MUST tolerate unknown
`veska_code` values (forward compat). The contract callers
rely on is the envelope shape; the `veska_code` is the key
into SOLO-16's catalogue.

### 4.6a What the editor sees during long-running daemon states

The daemon enters several states where it is up but not fully
serving: startup-resync (SOLO-03 §5.7), wake-reconcile (§5.2),
embedder-swap (§3.2), and crash-loop-recovery (§5.6). Editor
authors integrating MCP need to know what tools return in each
state so the surface can render usefully rather than appear
broken.

| Daemon state | Reads return | Writes return | Recommended editor handling |
|---|---|---|---|
| **Startup-resync** running for ≥1 repo | promoted (pre-resync) data with `degraded_reasons: ["startup_resync"]`; `eng_get_status` carries `commits_total`/`commits_done` per repo | `ErrDaemonStarting` with the same payload; caller may retry | Non-blocking progress chip ("Engram catching up: 5/12 commits"); poll `eng_get_status` every 2s; show read results with a "catching up" badge |
| **Wake-reconcile** running | promoted + (pre-sweep) staging with `degraded_reasons: ["wake_reconciling"]` | succeed normally; the sweep doesn't write | One-line non-blocking notice ("Engram re-syncing after sleep"); reads usable; clears within seconds |
| **Embedder-swap** running | promoted reads succeed; `eng_search_semantic` returns FTS5 lexical fallback with `degraded_reasons: ["embedder_swapping", "embedder_offline_lexical_fallback"]` | refused with `ErrUpstreamUnavailable`, `data.context.cause = "embedder_swapping"`; caller may retry once the swap state clears | Show "lexical-only search" badge; allow other reads |
| **Crash-loop tripped** | shim returns `ErrDaemonNotRunning`; `data.context.cli_command` = `veska doctor reset-crash-loop` | same | Surface the `cli_command` as a copyable block; same paste-handoff pattern as the human-action gate (SOLO-10 §3.3) |
| **Refuse-to-start** (sqlite-vec missing, schema mismatch, unsupported FS, etc.; SOLO-03 §5.8) | shim returns `ErrDaemonNotRunning` | same | Render `data.context.last_error` with an "open log file" affordance |

**Probe protocol.** The editor's MCP integration SHOULD poll
`eng_get_status` every 2s while any non-`ok` state is reported
and stop polling when status flips to `ok`. The polling
interval is not a contract; the boolean transition from
non-`ok` to `ok` is.

**Retry for `ErrDaemonStarting` and `ErrBusy`.** Both errors
are retryable. Recommended backoff: linear from 250ms to 2s over
8 retries, then poll `eng_get_status`. The error's
`data.context.eta_ms` (when present) is informative, not a
deadline.

New states added by future ADRs (e.g. an LLM-pipeline
cap-paused state) extend this table in place; the open
`degraded_reasons` vocabulary (§4.5) means new reasons can land
without breaking existing editor handling.

### 4.7 Write-tool blocking and `max_wait_ms`

All MCP write tools (`eng_close_finding`, `eng_reopen_finding`,
`eng_suppress_finding`, `eng_close_suppression`,
`eng_set_active_task`, `eng_add_repo`, `eng_remove_repo`) write
through the **`writeDB.hot`** pool (`*sql.DB` with
`MaxOpenConns=1`) alongside the promotion transaction. The pool is
the queue; SQLite's writer lock arbitrates against the embed
pool (SOLO-11 §10, ADR-S0011).

Every write tool accepts an optional `max_wait_ms: <int>`. The
default is `[mcp].write_max_wait_ms` (CONFIG-SURFACE; default
3000 ms for ordinary writes, 30000 ms for `eng_add_repo` /
`eng_remove_repo` whose work is cold-scan-bounded — those two
tools override at the handler level). When the deadline fires
the tool returns `ErrBusy` with `data.context.cause` set to
`"seal_in_flight"` or `"pool_wait"` and the corresponding
fields (SOLO-11 §10, ADR-S0011).

Once a write transaction has acquired `writeDB.hot` and begun
executing, it runs to completion regardless of the caller's
context. Cancellation prevents not-yet-started transactions from
acquiring the connection; in-flight transactions are not
interrupted.

## 5. Notes on specific tools

### 5.1 `eng_find_owner`

Engram has one user but its repos have many contributors. Owner
resolution combines two signals — the rules-declared owner and
the empirical owner — and returns both, because they are routinely
not the same person:

```jsonc
{
  "target": { "file": "internal/auth/oauth.go", "lines": [142, 198] },
  "codeowners": [
    { "owner": "@team-platform",   "rule": "internal/auth/" },
    { "owner": "@alice",           "rule": "internal/auth/oauth.go" }
  ],
  "blame": [
    { "author": "alice@corp.com",  "lines": 41, "last_touched": "2026-04-12" },
    { "author": "carol@corp.com",  "lines": 12, "last_touched": "2026-03-30" },
    { "author": "bob@corp.com",    "lines":  4, "last_touched": "2025-11-08" }
  ],
  "included_staging": true
}
```

**`codeowners`** is read from `.github/CODEOWNERS` /
`.gitlab/CODEOWNERS` / `CODEOWNERS` (in that order). The most
specific matching rule wins per the CODEOWNERS spec; we return
all matching rules so the caller can see the chain.

**`blame`** is `git blame -L <lines>` over the target lines (or
the whole file when no `lines` arg is given), aggregated by
author email, sorted by line count. We never return more than 10
authors; the long tail is collapsed into `"others": <count>`.

When the caller passes `target: { node_id: "..." }`, the lines
are derived from the node's `line_start..line_end`.

When the caller passes `target: { diff: { ref_a, ref_b } }`, the
lines are the diff's modified ranges. This is the "who else
recently touched what I'm changing" path.

The two signals are deliberately not merged. CODEOWNERS says "who
should review"; blame says "who actually knows this code."
Reviewers want both; agents want both.

### 5.2 `eng_get_context_pack` — bundle schema

`eng_get_context_pack` returns a single token-bounded JSON bundle
of the context an agent needs to start work. It shipped in M4
(`internal/application/contextpack`, handler in
`internal/infrastructure/mcp/tools_contextpack.go`).

**Input.** Beyond the required `repo_id` and `branch`, the caller
passes **exactly one** of `symbol` or `task_id`. Passing both, or
neither, is an `InvalidParams` error.

- **`{symbol}` mode** (`mode: "symbol"`) — the symbol name is
  resolved to its nodes (`FindNodes`); the relevant-nodes section
  is those seeds plus their blast radius.
- **`{task_id}` mode** (`mode: "task"`) — `domain.Task` carries no
  graph link, so the seed set is derived from the repo's
  **working-tree diff vs HEAD**: every changed file is mapped to
  the nodes it contains (`NodesInFile`), and the relevant-nodes
  section is that set plus its blast radius.

In both modes, recent commits, open findings and tasks are
derived from the resulting node set.

**Bundle shape.** The returned `Pack` carries `repo_id`,
`branch`, `mode`, `query` (the symbol or task ID), four content
sections, and the budget fields below:

- `nodes` — relevant nodes (seeds + blast radius), BFS-distance
  ordered. Each: `node_id`, `name`, `path`, `kind`, `distance`,
  `seed`, `has_open_finding`.
- `recent_commits` — distinct commits that touched the relevant
  nodes' files within the last 30 days, newest first. Each:
  `hash`, `author`, `when`, `subject`. The file walk is capped at
  25 files to bound git latency.
- `open_findings` — node IDs carrying an open finding. Each:
  `node_id`.
- `tasks` — the repo's active task, if any. Each: `task_id`,
  `repo_id`, `tracker`, `tracker_ref`, `title`, `active`.

**Token budget.** The bundle is clipped to a budget
(`token_budget`; default `DefaultTokenBudget = 8192`, overridable
at construction via `WithTokenBudget`). The estimate is
deterministic — `len(json_bytes) / 4` — and reported as
`estimated_tokens`. When the bundle is over budget, the
lowest-priority sections are dropped or clipped first, in order:
**Tasks → OpenFindings → RecentCommits → Nodes** (Tasks is
dropped whole; the rest are clipped from the tail until the
estimate fits). An oversized bundle is always truncated, never
rejected, and the response carries a `truncated` flag recording
whether anything was cut.

## 6. Tool descriptions

Every tool ships a description containing:

```
Returns:    <data shape>
Reads staging: yes | no
Active task: required | optional | not-used
Examples:
  - <one or two short example invocations>
```

Agents bind on the description as much as on the name. Description
changes are versioned with the tool.

## 7. Versioning and renames

Adding a tool, adding a non-required field, or relaxing a
constraint is non-breaking. Removing a tool, removing a field, or
tightening a constraint is breaking.

When a tool is renamed, the daemon registers an alias from the
old name to the new one for one minor release. The alias returns
the same payload; it emits a `Deprecation` header
(`X-Engram-Deprecation: <old-name> -> <new-name>`) on every call.
After one minor release the alias is removed.

There is no N+2 rule, no ADR-gated rename ceremony, no
machine-readable alias manifest. The CHANGELOG records renames;
that is the contract.

## 8. What's not in the surface

Tools that existed in earlier drafts and are not in §3. Cuts
split into three buckets: permanently dead, deferred until a
port or feature lands, and folded into something else.

### 8.1 Permanently dead

These won't return — either they belong upstream of a local
daemon, or the daemon's own design replaces them.

- `eng_get_pr_context` — PR review state lives in GitHub/GitLab. A separate MCP server (`gh`, `glab`) is the right home.
- `eng_get_session_recovery` — daemon restart already resumes from promoted state.
- `eng_describe_platform`, `eng_get_platform_health` — replaced by `eng_get_status`.
- `eng_estimate_task_complexity` — one-call derivation from `eng_get_dirty_blast_radius`.
- `eng_get_diff_test_impact` — one-call derivation from `eng_get_diff_blast_radius` filtered by `TESTS` edges.
- `eng_explain_edge` — debug tool; lives in `veska doctor` logs.
- `eng_list_open_todos` — folded into `eng_find_todos` with a `scope` arg.
- `eng_onboard_task` — replaced by `eng_get_context_pack`.
- `eng_get_node_as_of` — historical-query tool. Substrate stores latest-promoted only; no per-commit history. Returns when (and if) a history substrate ships. (`eng_find_changed_symbols`, originally deferred here, was reactivated by `solov2-4j5` — it parses two refs on demand and needs no substrate; it now lives in §3.1.)

### 8.2 Deferred until a port or feature lands

These come back when a real trigger fires:

| Tool group | Returns when |
|---|---|
| `eng_scan_vuln`, `eng_score_vuln`, `eng_explain_vuln`, `eng_prioritize_reachable_vuln` | `vuln-source` port ships an impl beyond findings flow. |
| `eng_get_coverage_for_node`, `eng_list_uncovered` | `coverage-source` port lands. |
| `eng_run_agent_specialty`, `eng_explain_agent_verdict`, `eng_get_agent_pr_preflight` | Review pipeline ships (M5). |
| `eng_render_wiki`, `eng_get_wiki_page`, `eng_search_wiki`, `eng_list_wiki_drift` | LLM synthesis returns to wiki. |
| `eng_find_dead_symbols`, `eng_rank_symbols`, `eng_risk_score`, `eng_find_duplicate_clusters`, `eng_explain_analysis_finding`, `eng_run_analysis_sweep` | Analysis port lands (likely M3 — `risk_score` is a real product feature). |
| `eng_list_tracker_issues`, `eng_get_tracker_issue`, `eng_create_tracker_issue`, `eng_link_tracker_issue`, `eng_update_tracker_issue`, `eng_close_tracker_issue` | Tracker read/write through MCP — likely M2/M3 once the agent flow needs it locally. |
| `eng_scan_secrets`, `eng_explain_secrets`, `eng_purge_secrets_mirror` | Secrets-scanner port through MCP (currently CLI-only). |
| `eng_list_contracts` | Contracts entity returns. |
| `eng_get_review_summary` | Once tracker read tools land — "what's the state of work in flight" is a real solo question on a multi-contributor team. |
| `eng_find_tasks_touching` | Same trigger as tracker read tools. Useful in a team: "is anyone else's open ticket touching this file?" |

### 8.3 Renamed or replaced

- `eng_search_workspace` → use `eng_find_symbol` / `eng_search_semantic` with `repo: "*"`.
- `eng_find_similar` → `eng_search_similar` (it's ranked; per the verb rule, that's a search).
- `eng_get_current_workspace` → `eng_get_current_repo` ("workspace" is banned vocab; SOLO-15).
- `eng_get_doctor_status` → `eng_get_status` (`doctor` is a CLI noun, not an MCP one).
- `eng_list_doctor_egress` → folded into `eng_get_config` (egress destinations are part of the config; redacted on the MCP path, available in clear via the `veska doctor config --show-secrets` CLI).

The §3 inventory is the surface; everything in this section is
why a tool you might expect is not in §3. Tool-by-milestone
breakdown lives in `milestones/M1.md` through `milestones/M5.md`.

## 9. Human-action gate

One gate:

```
eng_close_finding[severity >= high]   requires actor_kind = 'human'
```

`Severity` is ordered (`info < low < medium < high < critical`;
SOLO-04 §8.1) — the gate fires for `high` and `critical`.

Every other tool runs unconditionally for the daemon's user. The
gate is enforced server-side; failure returns
`ErrHumanActionRequired` with a `cli_command` payload (§4.6) so the
editor can render the close as a one-paste handoff (SOLO-10 §3.3).

`actor_kind` is set by the daemon from the accepting listener —
CLI socket → `'human'`, MCP socket → `'agent'`, daemon-internal
goroutine → `'system'`. A typical MCP client cannot supply or
override it.

The gate is a **UX and audit guardrail**, not a security
boundary. It catches the accidental MCP-side close ("the agent
called `eng_close_finding` while the human wasn't looking") and
keeps high-severity closures attributable in `audit.jsonl`. It
does not defend against same-user code, which can dial
`cli.sock` directly. The OS user is the privilege boundary;
SOLO-10 §3.1 has the full framing.
