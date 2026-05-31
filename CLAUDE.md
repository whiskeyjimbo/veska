# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->


## Build & Test

```bash
make build        # build veska, veska-daemon, veska-mcp (+ layercheck tool)
make test         # go test ./...
make all          # build + test + vet + lint + layercheck
make vet          # go vet ./...
make lint         # linter
make layercheck   # enforce hexagonal layering (domain/ports must not import infra)

go test ./internal/core/domain/...   # run a single package

# Eval / load-test harnesses — build tag `eval`. Some also need build tag
# `hnsw_native` (requires libusearch_c.so) and a running Ollama instance.
make eval-recall            # semantic_search recall@10 + p95
make eval-autolink-fp       # auto-link false-positive rate
make eval-revalidate-bench  # revalidation wall-time gate
make eval-queue-fuzz        # post-promotion queue lane drain
make eval-embed-throughput  # embedder throughput vs real Ollama
```

## Architecture Overview

Veska indexes a code graph (nodes + edges) and serves it to AI agents over MCP.
It follows **DDD-lite inside a hexagonal / ports-and-adapters shell**: tactical
DDD patterns (entities `Node`/`Edge`, aggregate root `Graph`, repository
interfaces, application services) without full strategic DDD. The core domain
never imports infrastructure — coupling flows inward through interfaces, and
`make layercheck` enforces it.

### Process topology — one binary, three personalities

`make build` produces a single Go binary at `bin/veska`; `bin/veska-daemon` and
`bin/veska-mcp` are symlinks to it. The argv[0] dispatcher in
`cmd/veska/main.go` routes the daemon and MCP-shim invocations into their own
packages:

- `veska` — CLI; entry point `cmd/veska/main.go`, manual DI wiring inside each
  subcommand's `RunE`.
- `veska-daemon` (symlink) — long-running process; argv[0] dispatch routes
  into `internal/cli/daemon`, whose `wire.go` is the composition root. Owns
  the MCP Unix-socket server, fsnotify watcher, embedder worker, and
  post-promotion queue poller.
- `veska-mcp` (symlink) — thin stdio shim; argv[0] dispatch routes into
  `internal/cli/mcp`, which proxies an editor's JSON-RPC frames to the
  daemon's Unix socket.

### Layout

```
cmd/veska/                <- single binary entry point; argv[0] dispatcher in main.go
internal/
  core/
    domain/               <- pure entities: Node, Edge, Graph, Task
    ports/                <- interface contracts only (GraphStorage, VectorStorage,
                             EmbeddingProvider, CodeParser, EdgeReader, NodeLookup, ...)
  application/            <- use-case services: Ingester, Promoter, embedder, autolink,
                             revalidate, search, blastradius, checks
  cli/
    daemon/               <- daemon composition root (wire.go); fsnotify+queue+MCP server
    mcp/                  <- stdio MCP shim that proxies to the daemon's Unix socket
  infrastructure/
    sqlite/               <- graph + queue + FTS storage; PromotionStore + sinks
    vector/               <- dual-backend VectorStorage (sqlite-vec default,
                             usearch/float16 HNSW via the hnsw_native build tag)
    embedding/ollama/     <- Ollama EmbeddingProvider adapter
    treesitter/           <- CodeParser
    mcp/                  <- MCP server + tool families
    git/                  <- fsnotify watcher, diff reader, hook install
  repo/                   <- repos-table registry (Add/Remove/List + git hooks)
  platform/               <- cross-cutting operational concerns. Two altitudes
                             coexist here by design: zero-dep leaves imported
                             widely (config, tokenize, logrotate, crashloop,
                             observability, service, embedderprobe, health) and
                             doctor — a high-altitude diagnostic consumer that
                             imports application + infrastructure to assemble
                             health bundles. layercheck permits this: platform
                             is operational tooling, not an inner layer. doctor
                             stays here (rather than internal/diagnostics) since
                             relocating it churns every importer for a naming
                             nicety with no dependency benefit .
```

### Runtime dependencies

| Dependency | Purpose | Notes |
|---|---|---|
| SQLite (`github.com/mattn/go-sqlite3`, in-proc, cgo + `sqlite_fts5`) | graph + queue + FTS storage | no server process. Pinned via `internal/infrastructure/sqlite/sqldriver/`; chosen for the 1.6–2.5× speedup over modernc on driver-bound workloads . The pure-Go modernc opt-in was removed  because tree-sitter requires cgo anyway, so the no-cgo cross-compile story it preserved did not actually exist. |
| sqlite-vec / usearch | vector storage | sqlite-vec default; usearch HNSW above the M2 threshold (`hnsw_native` tag + `libusearch_c.so`) |
| Ollama | local embeddings | `VESKA_OLLAMA_URL` (default `http://localhost:11434`), `VESKA_EMBED_MODEL` (default `nomic-embed-text`) |

Env: `VESKA_HOME` (data root), `VESKA_VECTOR_BACKEND` (`sqlite-vec`|`usearch`).

### Key design decisions

- **Functional options** for domain constructors — `NewNode(id, ...NodeOption)`.
- **Constructors return typed errors**, not panics, on a nil/invalid dependency
  (each package exposes an `ErrMissingDependency` sentinel).
- **Ports are owned by their consumer.** Two homes, by scope: broad,
  cross-cutting storage/provider contracts live in `internal/core/ports/`
  (`GraphStorage`, `VectorStorage`, `EmbeddingProvider`, `CodeParser`, …) — the
  domain/application core depends on these. Narrow, use-case-specific ports are
  declared in the `application/*` (or other consumer) sub-package that uses
  them, sized to exactly the methods that consumer needs — e.g.
  `application.PromotionStore`, `wiki.RenderTimeStore`, `review.DailyTokenStore`,
  `embedder.EmbedRefQueue` (an ISP-narrowed view of the fat
  `ports.EmbeddingRefRepo`; solov2-xde2.16). A single infrastructure adapter
  typically satisfies both the broad port and several consumer-owned interfaces.
  Rule of thumb: if only one use case needs it, the interface belongs next to
  that use case, not in `core/ports` .
- **Promotion is atomic.** `Promoter` (application) is a thin orchestrator; all
  SQL lives behind the `application.PromotionStore` port, implemented by
  `sqlite.PromotionStore`. Co-transactional writers (FTS, embedding-refs) are
  pluggable `sqlite.PromotionSink`s registered in `wire.go` — adding one needs
  no edit to the transaction body.
- **Embeddings are L2-normalised** before storage so the raw vector
  similarity `1/(1+L2dist)` lands in a usable threshold range. **That
  formula governs the autolink threshold only.** `eng_search_semantic`
  exposes a post-fusion RRF score (sum of `1/(60+rank)` across the vector
  and lexical lists), not the raw similarity — top scores cluster around
  ~0.016–0.033 by construction, and are only meaningful relative to other
  hits in the same query .

## Conventions & Patterns

- Commit messages: one-line conventional commits (`type(area): description`);
  **never** add a `Co-Authored-By` trailer.
- Run `go fmt ./... && go fix ./...` before every commit.
- New behaviour reuses the existing DDD-lite patterns — do not introduce new
  architectural patterns.
- Load/eval harnesses live under `tools/loadtest/` behind the `eval` build tag
  so `go test ./...` stays fast.
- The ubiquitous language (`Node`, `Edge`, `Graph`, `Task`) stays consistent
  across domain, ports, adapters, and CLI output.
- **One file per concern, flat within a package.** `internal/infrastructure/mcp`
  (one `tools_*.go` per tool family) and `internal/infrastructure/sqlite` (one
  file per repo/concern) are deliberately flat, even at 60+ / 40+ files — the
  cohesion of a single package outweighs directory nesting, and the file names
  make navigation predictable. This is an affirmed convention, not drift; do
  not split these into subpackages absent a concrete coupling problem
  .
