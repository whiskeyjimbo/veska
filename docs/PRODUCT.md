---
verified: true
verified_date: "2026-06-19"
---

# Veska - Product Narrative

> Reader-friendly. This file is for new readers and anyone deciding
> whether to use Veska. The technical reference is
> [`docs/ARCHITECTURE.md`](ARCHITECTURE.md); operational detail lives
> under [`docs/operations/`](operations/).

## What it is

Veska is a **local code-intelligence daemon**. It runs on your
laptop, parses your repo into a graph, embeds the graph
semantically, and exposes both through MCP so your editor and your
AI agent see the same model of the codebase.

That is the entire product. There is no upstream. There is no
shared service. There is no multi-tenant tier. The data lives in
`~/.veska/` on your machine; backup is `veska backup create`
(SQLite online snapshot + a tarball).

## What it gives you

- **Grounded structural answers.** Every reference to a function,
  type, file, or call traces to a node, an edge, a commit, or a
  file. Your agent has a real structural ground truth to check
  against instead of guessing function names. Structural recall
  converges within the save→staging freshness window: the save
  event, fsnotify debounce, and tree-sitter reparse. Sub-second on a
  quiet laptop with small files; longer on macOS (FSEvents coalesces)
  or on large files (parse cost dominates). Not a hard real-time
  guarantee. Staging is in-memory and volatile; a daemon crash
  before commit drops unpromoted parses, which the next save
  reproduces.
- **Eventually-consistent semantic answers.** `eng_search_semantic`
  is the one part of the surface that runs on a different clock.
  With the default in-process embedder (model2vec), embedding is
  fast - microseconds per symbol - so the lag is bounded mainly by
  how quickly the post-promotion queue drains, typically seconds.
  (The *optional* Ollama embedder is far slower: a refactor commit
  can take minutes to hours, depending on the model and CPU.) During
  the lag window `eng_search_semantic` falls back to a lexical index
  over symbol names and tags the response `degraded_reasons:
  ["embeddings_pending"]` or `["embedder_offline_lexical_fallback"]`.
  We do not promise point-in-time semantic recall; we promise
  structural recall is always current and semantic recall catches up
  quickly.
- **Cross-actor attribution.** When the agent edits a file and you
  edit a file, the audit log can tell them apart. (One enum:
  `actor_kind: human | agent | system`. That is the entire
  identity model.)
- **Pluggable signals.** Vuln feeds, coverage data, and ownership
  hints attach to the same graph through a small set of Go
  interfaces.

## How it works

The product is one daemon (`veska-daemon`) plus two thin
clients. The daemon owns one SQLite file under `~/.veska/`. The
default embedder runs **in-process** (model2vec), so the default
config makes **no outbound connection at all**. Ollama is optional
- it backs the off-by-default LLM review pipeline (and an opt-in
embedder override via `VESKA_EMBEDDER=ollama`) - and is the only
outbound connection *when enabled*.

**System view - three boxes.**

```
┌─ developer machine ─────────────────────────────────────┐
│                                                         │
│   ┌──────────────┐    unix sockets    ┌──────────────┐  │
│   │  client      │ ─────────────────▶ │   veska-     │  │
│   │  surfaces    │                    │   daemon     │  │
│   │              │                    │              │  │
│   │  • CLI       │                    │  owns SQLite │  │
│   │  • editor    │                    │  + memvec,   │  │
│   │    MCP shim  │                    │  fsnotify,   │  │
│   └──────────────┘                    │  embed/promo │  │
│                                       │  goroutines  │  │
│                                       └───┬──────┬───┘  │
│                                           │      │ HTTP │
│                                           ▼      ▼      │
│                                    ┌─────────┐ ┌──────┐ │
│                                    │~/.veska │ │Ollama│ │
│                                    └─────────┘ └──────┘ │
└─────────────────────────────────────────────────────────┘
```

**[client surfaces] ↔ [daemon] → [SQLite file] (+ optional Ollama).**
The daemon is the only component with state. In the default config
there is no outbound connection; the `Ollama` box above appears only
when the review pipeline or a forced Ollama embedder is enabled.

**A working install runs:** `veska-daemon`, the `veska` CLI, the
`veska-mcp` stdio shim, an OS-level supervisor (launchd /
systemd-user / the built-in supervisor), and your editor -
**plus Ollama only if you enable the LLM review pipeline.** The
diagram above collapses CLI + editor into "client surfaces" and
elides the supervisor for narrative clarity, but the supervisor is
**load-bearing** for first-run UX, the orphan-process refusal, and
the crash-loop story - it is not optional. The
[Supervision Runbook](operations/SUPERVISION-RUNBOOK.md) carries the
canonical topology and recovery steps.

There are three flows through this picture:

**Save** - the hot path. Runs in RAM, answers in milliseconds.

```
editor writes file
      │
      ▼
 fsnotify ──► tree-sitter parse ──► staging (in-memory)
                                          │
                                          ▼
                                 answers via MCP (synchronous)
```

**Promotion** - durable, on `git commit`. The only path that touches disk for writes. ("Promote" is the verb, "promotion" the transaction, "promoted" the adjective for durable rows.)

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
                                  embedder ──► memvec
```

**Query** - MCP reads through both layers. Tools declare which freshness they provide.

```
editor / agent
      │
      ▼
veska-mcp ──► unix socket ──► router ──┬──► staging (sees unpromoted edits)
                                        │
                                        └──► SQLite + memvec
                                              (promoted state;
                                               may report degraded)
```

**Read merge - per-file overlay, not per-row.** When staging has
an entry for a file, that file's staging rows fully replace the
promoted rows for the same `file_path` on the active branch; promoted
rows for that file are excluded entirely. There is no
three-way row-level merge. A traversal can therefore mix promoted
rows (from clean files) with staging rows (from dirty files)
across the *file* boundary, but within any single file the source
is one or the other.

**History-rewrite operations short-circuit.** During `git rebase`,
`git merge --continue`, `git cherry-pick`, and `git bisect` the
post-commit hook detects the operation marker and returns 0
without promoting; catch-up runs on the next clean commit.
`git commit --amend` falls through the divergent-promotion path:
the original commit's SHA is unreachable from the new HEAD,
`ErrPromotionDivergent` is logged, and the daemon re-parses the
working tree. Bulk-replay storms (a 10-commit rebase fires 10 hooks
in seconds) collapse into a single catch-up walk.

The process topology, the save/promote split, and the read-merge
rule are documented in [`docs/ARCHITECTURE.md`](ARCHITECTURE.md)
§2 and §5.

## What it is not

- A visual dashboard. Visualization belongs in your editor.
- A primary truth derived from LLMs. Call edges, blast radius, and
  containment come from tree-sitter and the parser. LLMs summarise
  and review; they don't replace the structural graph.
- A distributed database. SQLite is one. Veska leans on it.
- A multi-machine product. If you want to share intelligence
  across a team, that is out of scope.
- An identity-as-a-product offering. Identity here is "the user
  who started the daemon" plus an `actor_kind` enum.
- A general-purpose audit/compliance system. The audit log is an
  append-only `audit.jsonl`. Forward it to whatever you like.
- A secret-recovery tool. Veska surfaces secret leaks; rewriting
  Git history and rotating credentials is on you (see the
  [Secrets Runbook](operations/SECRETS-RUNBOOK.md)).

## Privacy & telemetry

**Zero telemetry leaves your machine by default.** No crash
reports. No usage analytics. No model-call logs. The flip side:
**when the daemon misbehaves, no one - including you - has data
to diagnose unless you opted in.** `veska doctor` and
`veska doctor bundle` are the on-demand operator surfaces; if you
want proactive "is my daemon OK?" telemetry, you wire it up
yourself (Prometheus + your own scraper + your own dashboard).

You opt in to egress explicitly:

- An OTLP trace endpoint (`VESKA_OTLP_ENDPOINT`).
- A Prometheus `/metrics` endpoint.
- A vendor LLM. **Ships only the local Ollama generator.**
  Hosted providers (Anthropic, OpenAI, Gemini, OpenAI-compatible)
  are deferred behind a future decision. The review pipeline honors
  hard halts on tokens-per-commit and tokens-per-day (`[review]`
  table in your config); USD caps come with hosted providers when
  they ship.
- A vuln feed source (`[vuln_source]`; `veska init` writes
  `provider = "osv"` by default, opt out with `veska init --no-vuln`).

`veska doctor` (with `--json` for machine-readable output)
lists every configured outbound destination under its `egress`
section so you can audit at any time. If something goes wrong
and you want to share state without uploading anything yourself,
`veska doctor bundle` writes a single `.tar.gz` to disk that you can
attach to a GitHub issue. Source code, node bodies,
embedding bytes, and LLM payloads are excluded; secret-shaped
config keys are redacted. The bundle is operator data, not
sanitised data - open it before sharing. Nothing leaves your
machine until you send the file yourself.

## What it costs

Veska runs on a commodity laptop (8-core, 16 GB, NVMe). The
performance characteristics that matter - parse latency, embedding
throughput, write contention, and RSS - are measured rather than
promised in the abstract; the embedder numbers live in
[`docs/operations/embedder-benchmarks.md`](operations/embedder-benchmarks.md).

**Vector substrate.** Semantic search runs over a single elected
embedding model (model2vec / potion-code-16M by default; see
[`docs/ARCHITECTURE.md`](ARCHITECTURE.md) §3). Vectors are served two
ways:

- **`memory` (memvec, default)** - an in-process linear scan. Zero
  native dependency, fine for a typical working set.
- **`usearch` (opt-in)** - an HNSW index for larger graphs, behind
  the `hnsw_native` build tag and `libusearch_c.so`. Selected via
  `VESKA_VECTOR_BACKEND=usearch`.

`veska doctor storage` reports headroom (RSS against the
`[memory]` soft/hard caps) so the long-tail user whose working set
outgrows the linear scan sees it before it bites, and can switch to
`usearch`.

## Getting started

```
veska init
```

Run it from inside a Git working tree. `veska init` writes the
config, reports the **elected in-process embedder** (model2vec if
installed or compiled into the binary, else the built-in static-v2
fallback), registers the daemon with your session manager, and
registers the current repo. A short summary at the end (data dir,
embedder, service status, registered repos) tells you what
happened.

**No external service is required.** If only the static-v2 fallback
is available, init suggests `veska install model2vec` for
higher-quality code search - a one-time ~62 MB download, or compile
it into the binary with `make build-fat`. Only an explicit
`VESKA_EMBEDDER=ollama` makes init probe Ollama and exit non-zero
when it is unreachable.

## Platforms

- macOS (Apple silicon, x86-64): supported.
- Linux (x86-64): supported.
- Windows: not supported. WSL2 likely works via the Linux unit
  but is untested and unsupported.

## Where the detail lives

- [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) - process topology,
  storage substrate, hexagonal layering, save vs. promotion clocks.
- [`docs/operations/CONFIG-SURFACE.md`](operations/CONFIG-SURFACE.md)
  - the config file and environment variables.
- [`docs/operations/SUPERVISION-RUNBOOK.md`](operations/SUPERVISION-RUNBOOK.md)
  - install, upgrade, crash-loop recovery.
- [`docs/operations/SECRETS-RUNBOOK.md`](operations/SECRETS-RUNBOOK.md)
  - what to do when a secret-scan fires.
- [`docs/operations/embedder-benchmarks.md`](operations/embedder-benchmarks.md)
  - measured recall and latency per embedding model.
- `docs/manual/` - the user manual.
