# Veska — Product Narrative

> Reader-friendly. The Charter (`design/01-charter/`) is binding;
> this file is for new readers and anyone deciding whether to use it.

> **Design status.** Draft. Numbered budgets in the design tree are
> labelled `BUDGET (unmeasured) | (measured M<N>) | INVARIANT |
> DEFAULT` (SOLO-13 §3); unmeasured budgets are gated by milestone
> measurement and may be revised. Approval comes after M1 ships
> with real numbers.

## What it is

Engram is a **local code-intelligence daemon**. It runs on your
laptop, parses your repo into a graph, embeds the graph
semantically, and exposes both through MCP so your editor and your
AI agent see the same model of the codebase.

That is the entire product. There is no upstream. There is no
shared service. There is no multi-tenant tier. The data lives in
`~/.veska/` on your machine; backup is `veska backup create`
(SQLite online snapshot + a tarball; see
`design/08-data-and-storage/` §9).

## What it gives you

- **Grounded structural answers.** Every reference to a function,
  type, file, or call traces to a node, an edge, a commit, or a
  file. Your agent has a real structural ground truth to check
  against instead of guessing function names. Structural recall
  converges within the save→staging freshness budget (SOLO-13
  §3.1b): the save event, fsnotify debounce, and tree-sitter
  reparse. Sub-second on a quiet laptop with small files; longer
  on macOS (FSEvents coalesces) or on large files (parse cost
  dominates). Not a hard real-time guarantee — a measured budget.
  Staging is in-memory and volatile; a daemon crash before commit
  drops unpromoted parses, which the next save reproduces.
- **Eventually-consistent semantic answers.** `semantic_search`
  is the one part of the surface that runs on a different clock.
  Embedding throughput depends on (a) which Ollama model you
  configured, (b) your CPU class, (c) what else is running. A
  small commit on an idle laptop with a quantised model
  embeds in seconds; a refactor commit on a busy laptop with
  full `nomic-embed-text` can run for minutes to hours. During
  the lag window `semantic_search` falls back to a BM25 lexical
  index over symbol names and tags the response
  `degraded_reasons: ["embedding_pending"]` or
  `["embedder_offline_lexical_fallback"]`. SOLO-13 §3.2 carries
  the matrix once M3 measures it. We do not promise
  point-in-time semantic recall; we promise structural recall
  is always current and semantic recall catches up **when Ollama
  can keep pace**. If your refactor velocity sustainedly exceeds
  your CPU's embedding throughput, expect a perpetual lag — the
  daemon emits a sticky `embed-deferred-saturated` finding when
  the deferred-embed queue's oldest row ages past 24h
  (SOLO-08 §3.4) so the condition is visible rather than silent.
- **Cross-actor attribution.** When the agent edits a file and you
  edit a file, the audit log can tell them apart. (One enum:
  `actor_kind: human | agent | system`. That is the entire
  identity model.)
- **Pluggable signals.** Vuln feeds, coverage data, and ownership
  hints attach to the same graph through a small set of Go
  interfaces.

## How it works

The product is one daemon (`veska-daemon`) plus two thin
clients. The daemon owns one SQLite file under `~/.veska/`.
Ollama runs as a separate user process and is the only outbound
connection in the default config.

**System view — three boxes.**

```
┌─ developer machine ─────────────────────────────────────┐
│                                                         │
│   ┌──────────────┐    unix sockets    ┌──────────────┐  │
│   │  client      │ ─────────────────▶ │   veska-    │  │
│   │  surfaces    │                    │   daemon     │  │
│   │              │                    │              │  │
│   │  • CLI       │                    │  owns SQLite │  │
│   │  • editor    │                    │  + vec0,     │  │
│   │    MCP shim  │                    │  fsnotify,   │  │
│   └──────────────┘                    │  embed/promotion  │  │
│                                       │  goroutines  │  │
│                                       └───┬──────┬───┘  │
│                                           │      │ HTTP │
│                                           ▼      ▼      │
│                                    ┌─────────┐ ┌──────┐ │
│                                    │~/.veska│ │Ollama│ │
│                                    └─────────┘ └──────┘ │
└─────────────────────────────────────────────────────────┘
```

**[client surfaces] ↔ [daemon] → [SQLite file] + [Ollama].**
The daemon is the only component with state. Ollama is the only
outbound connection in the default config.

**Six runtime pieces, one stateful component.** A working
install runs: `veska-daemon`, the `veska` CLI, the
`veska-mcp` stdio shim, an OS-level supervisor (launchd /
systemd-user / the built-in `veska supervise`), Ollama, and
your editor. The diagram above collapses CLI + editor into
"client surfaces" and elides the supervisor for narrative
clarity, but the supervisor is **load-bearing** for first-run
UX, the orphan-process refusal, and the crash-loop story — it is
not optional. SOLO-03 §3 carries the canonical topology with
all six pieces named.

There are three flows through this picture:

**Save** — the hot path. Runs in RAM, answers in milliseconds.

```
editor writes file
      │
      ▼
 fsnotify ──► tree-sitter parse ──► staging (in-memory)
                                          │
                                          ▼
                                 answers via MCP (synchronous)
```

**Promotion** — durable, on `git commit`. The only path that touches disk for writes. ("Promote" is verb, "promotion" is the transaction, "promoted" is the adjective for durable rows; the [glossary](design/15-glossary/README.md#promotion--the-term-family) is the disambiguation home.)

```
git commit
      │
      ▼
post-commit hook ──► daemon drains staging ──► SQLite tx (atomic)
                                                     │
                                                     ▼
                                              post_promotion_queue (queue)
                                                     │
                                     ┌───────────────┼───────────────┐
                                     ▼               ▼               ▼
                                  embed          auto-link      revalidate
                                  worker
                                     │
                                     ▼
                                  Ollama ──► sqlite-vec
```

**Query** — MCP reads through both layers. Tools declare which freshness they provide.

```
editor / agent
      │
      ▼
veska-mcp ──► unix socket ──► router ──┬──► staging (sees unpromoted edits)
                                        │
                                        └──► SQLite + sqlite-vec
                                              (promoted state;
                                               may report degraded)
```

**Read merge — per-file overlay, not per-row.** When staging has
an entry for a file, that file's staging rows fully replace the
promoted rows for the same `file_path` on the active branch; promoted
rows for that file are excluded entirely. There is no
three-way row-level merge. A traversal can therefore mix promoted
rows (from clean files) with staging rows (from dirty files)
across the *file* boundary, but within any single file the source
is one or the other. SOLO-11 §1.2 is normative.

**History-rewrite operations short-circuit.** During `git rebase`,
`git merge --continue`, `git cherry-pick`, and `git bisect` the
post-commit hook detects the operation marker and returns 0
without promoting; catch-up runs on the next clean commit.
`git commit --amend` falls through the standard divergent-promotion
path (SOLO-11 §2.3): the original commit's SHA is unreachable
from the new HEAD, `ErrPromotionDivergent` is logged, and the daemon
re-parses the working tree. Bulk-replay storms (10-commit rebase
fires 10 hooks in seconds) collapse into a single catch-up walk.

Process topology and supervision are in
[`design/03-the-daemon/`](design/03-the-daemon/README.md); the
save/promote split, the merge rule, and the history-rewrite handling
are in [`design/11-pipelines/`](design/11-pipelines/README.md)
§1.2 and §2.3.

## What it is not

- A visual dashboard. Visualization belongs in your editor.
- A primary truth derived from LLMs. Call edges, blast radius, and
  containment come from tree-sitter and the parser. LLMs summarise
  and review; they don't replace the structural graph.
- A distributed database. SQLite is one. Engram leans on it.
- A multi-machine product. If you want to share intelligence
  across a team, that is parked in `deferred/`.
- An identity-as-a-product offering. Identity here is "the user
  who started the daemon" plus an `actor_kind` enum.
- A general-purpose audit/compliance system. The audit log is an
  append-only `audit.jsonl`. Forward it to whatever you like.
- A secret-recovery tool. Engram surfaces secret leaks; rewriting
  Git history and rotating credentials is on you.

## Privacy & telemetry

**Zero telemetry leaves your machine by default.** No crash
reports. No usage analytics. No model-call logs. The flip side:
**when the daemon misbehaves, no one — including you — has data
to diagnose unless you opted in.** `veska doctor` and
`veska bundle` are the on-demand operator surfaces; if you want
proactive "is my daemon OK?" telemetry, you wire it up yourself
(Prometheus + your own scraper + your own dashboard).

You opt in to egress explicitly:

- An OTLP trace endpoint (`VESKA_OTLP_ENDPOINT`).
- A Prometheus `/metrics` endpoint.
- A vendor LLM. **Ships only the local Ollama generator.**
  Hosted providers (Anthropic, OpenAI, Gemini, OpenAI-compatible)
  are deferred behind an ADR + measurement of the local
  pipeline. The review pipeline honors hard halts on
  tokens-per-commit and tokens-per-day (`[review]` table in your
  config); USD caps come with hosted providers when they ship.
- A vuln feed source (configure `vuln-source`; default is none).

`veska doctor` (with `--json` for machine-readable output)
lists every configured outbound destination under its `egress`
section so you can audit at any time. If something goes wrong
and you want to share state without uploading anything yourself,
`veska bundle` writes a single `.tar.gz` to disk that you can
attach to a GitHub issue. Source code, node bodies,
embedding bytes, and LLM payloads are excluded; secret-shaped
config keys are redacted. The bundle is operator data, not
sanitised data — open it before sharing (SOLO-13 §2.2 spells
out exactly what redaction does and doesn't catch). Nothing
leaves your machine until you send the file yourself.

## What it costs

Engram runs on a commodity laptop (8-core, 16 GB, NVMe). Budgets and
the gates that turn them into measurements live in
[`design/13-nfr/`](design/13-nfr/README.md) §3. One framing worth
knowing: every performance number in the design tree is labelled
(`BUDGET (unmeasured)` / `BUDGET (measured M<N>)` / `INVARIANT` /
`DEFAULT`). Unmeasured budgets are targets, not decisions; SOLO-13
§3 is the canonical home and any number elsewhere cross-references
it.

**Vector substrate at M1 — gated on M0 measurement.** The plan
is to ship brute-force `vec0` for `semantic_search` and
`find_similar_symbols` at M1, with HNSW as the M2 pivot. That
plan is **conditional on M0's measurement** (epic m0.01) — not
a foregone conclusion:

- M0 measures the vec0 ceiling on the reference laptop: the node
  count at which brute-force vec0 misses either the
  `semantic_search` p95 budget or the 2 GiB soft RSS cap.
- If the measured ceiling clears `[ceiling].minimum_for_m1`
  (DEFAULT 250 000 nodes — matched to M0's "Red — ceiling"
  trigger), M1 ships vec0 and the runtime guard below catches
  the long-tail user whose working set runs over.
- If the measured ceiling falls below the floor, **M1 does not
  ship vec0**. The HNSW pivot moves into M1 scope and M1's exit
  gate slides. M1 will not ship a substrate the design says
  cannot serve a typical working set.

Either way, the runtime guard ships at M1: `veska doctor
storage` reports headroom from the first day, and
`semantic_search` returns
`degraded_reasons: ["vec0_ceiling_warn" | "vec0_ceiling_exceeded"]`
as the user approaches or crosses the measured ceiling. SOLO-13
§3.3.1 is the normative version of this gate; this paragraph is
the user-facing summary.

## Getting started

```
veska init
```

Run it from inside a Git working tree. `veska init` writes the
config, probes for Ollama (and offers to `ollama pull
nomic-embed-text` if the model is missing), registers the daemon
with your session manager, and registers the current repo. A
short summary at the end (data dir, config path, embedder
status, service status, registered repos, tracker status, audit
log path) tells you what happened.

If Ollama isn't installed, init prints the install command for
your platform and exits non-zero. Engram does not bundle Ollama;
it does not ship an alternative embedder. The dependency is real
and explicit.

## Platforms

- macOS (Apple silicon, x86-64): supported.
- Linux (x86-64): supported.
- Windows: not supported. WSL2 likely works via the Linux unit
  but is untested and unsupported.

## Where the detail lives

- `design/01-charter/` — binding pillars.
- `design/03-the-daemon/` — runtime topology.
- `design/04-domain-model/` — entities, ports, invariants.
- `design/08-data-and-storage/` — SQLite substrate.
- `design/11-pipelines/` — save vs. promotion flow.
- `design/09-mcp-surface/` — editor-facing API.
- `design/13-nfr/` — observability and performance budgets.
- `design/adr/` — decisions worth recording.
- `milestones/` — PR-sized work breakdown.

The Charter is the contract; this file is the trailer.
