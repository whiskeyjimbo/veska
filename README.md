# Veska

**Veska** is a local code-intelligence daemon. It runs on your laptop, parses
your repository into a code graph (nodes + edges), embeds that graph
semantically, and serves both to your editor and your AI agent over MCP — so
they reason from the same structural ground truth instead of guessing.

> "Engram" is the product name; "Veska" is the implementation and the binary
> name. One daemon, one developer, one machine. There is no upstream, no shared
> service, no multi-tenant tier — everything Veska knows lives in `~/.veska/`.

## What it gives you

- **Grounded structural answers.** Every function, type, file, and call traces
  to a node, edge, or commit. Structural recall stays current within the
  save → staging freshness budget.
- **Eventually-consistent semantic search.** `semantic_search` embeds the graph
  via Ollama; during the lag window it falls back to a BM25 lexical index and
  flags the response `degraded_reasons`.
- **Promotion checks.** On every commit, synchronous checks emit advisory
  `Finding`s: dead code, contract drift, vulnerable `go.mod` dependencies
  (OSV), and leaked secrets.
- **Optional LLM review.** An off-by-default post-promotion review pipeline.
- **Mechanical wiki.** Hot-zone and entry-point pages plus a context-pack tool,
  computed from the graph — no LLM in the path.
- **Cross-actor attribution.** A single `actor_kind: human | agent | system`
  enum distinguishes who changed what in the audit log.

## Process topology — three binaries

| Binary | Role |
|---|---|
| `veska` | CLI — `init`, `status`, `doctor`, `backup`, `wiki`, … |
| `veska-daemon` | Long-running process — owns the SQLite store, the fsnotify watcher, the embedder, and the post-promotion queue. Composition root: `cmd/veska-daemon/wire.go`. |
| `veska-mcp` | Thin stdio shim proxying an editor's MCP connection to the daemon's Unix socket. |

## Requirements

- **Go 1.26+**
- **[Ollama](https://ollama.com)** running locally with an embedding model
  (default `nomic-embed-text`). The only outbound connection in the default
  config. The optional review pipeline also uses Ollama.
- SQLite and the vector index are in-process — no server to run.

## Build

```sh
make build        # build veska, veska-daemon, veska-mcp (+ layercheck tool)
make test         # go test ./...
make all          # build + test + vet + lint + layercheck
make layercheck   # enforce hexagonal layering (domain/ports must not import infra)
```

## Quick start

```sh
ollama serve &                       # if not already running
ollama pull nomic-embed-text
veska init                           # creates ~/.veska/, probes the embedder
veska-daemon &                       # start the long-running daemon
veska repo add /path/to/your/repo    # register; cold-scan kicks off in the background
```

The first `veska repo add` registers the repo, installs the git post-commit
hook with an absolute path to the `veska` binary, and dispatches a cold scan
through the daemon. Subsequent commits drive promotion via `eng_promote_repo`
on the daemon's MCP socket. Point your editor's MCP client at `veska-mcp`.
Check health with `veska doctor status` and the MCP `eng_get_status` tool.

To force a re-scan of an already-registered repo (e.g. after a model swap):

```sh
veska reindex /path/to/your/repo
```

### Configuration

State lives under `~/.veska/` (`VESKA_HOME`). Daemon config is
`~/.veska/config.toml` — see [`docs/operations/CONFIG-SURFACE.md`](docs/operations/CONFIG-SURFACE.md).
Key environment variables:

| Var | Purpose | Default |
|---|---|---|
| `VESKA_HOME` | Data root | `~/.veska` |
| `VESKA_OLLAMA_URL` | Ollama endpoint | `http://localhost:11434` |
| `VESKA_EMBED_MODEL` | Embedding model | `nomic-embed-text` |
| `VESKA_VECTOR_BACKEND` | `sqlite-vec` or `usearch` | `sqlite-vec` |

## Architecture

Veska follows **DDD-lite inside a hexagonal / ports-and-adapters shell**: the
core domain never imports infrastructure — coupling flows inward through
interfaces, and `make layercheck` enforces it.

```
cmd/                  the three binaries; veska-daemon/wire.go is the composition root
internal/
  core/
    domain/           pure entities: Node, Edge, Graph, Task, Finding
    ports/            interface contracts (GraphStorage, VectorStorage, VulnSource, …)
  application/        use-case services: ingester, promoter, embedder, checks, review, wiki
  infrastructure/     adapters: sqlite, vector, embedding/ollama, treesitter, mcp, git
  repo/               repos-table registry
docs/                 design set (SOLO-NN sections), milestones, operations runbooks
```

Design decisions of note: functional options for domain constructors;
constructors return typed errors (never panic) on missing dependencies; atomic
promotion behind the `PromotionStore` port; L2-normalised embeddings.

## MCP tools

The daemon exposes 34 tools over a Unix-socket JSON-RPC server (forwarded to
editors by `veska-mcp`). Tool names follow `eng_<verb>_<object>`. Quick map:

| Family | Tools |
|---|---|
| Admin | `eng_get_status`, `eng_get_config`, `eng_get_current_repo`, `eng_get_repo`, `eng_list_repos` |
| Repo lifecycle | `eng_add_repo`, `eng_remove_repo`, `eng_promote_repo` |
| Graph | `eng_find_symbol`, `eng_get_node`, `eng_get_file_nodes`, `eng_get_call_chain` |
| Search | `eng_search_semantic`, `eng_search_similar` |
| Blast radius | `eng_get_blast_radius`, `eng_get_diff_blast_radius`, `eng_get_dirty_blast_radius` |
| Context | `eng_get_context_pack`, `eng_find_changed_symbols` |
| Misc | `eng_find_owner`, `eng_find_todos` |
| Tasks | `eng_get_active_task`, `eng_set_active_task`, `eng_get_task_history` |
| Findings | `eng_list_findings`, `eng_get_finding`, `eng_close_finding`, `eng_reopen_finding` |
| Suppressions | `eng_list_suppressions`, `eng_get_suppression`, `eng_suppress_finding`, `eng_close_suppression` |
| Wiki | `eng_get_hot_zone`, `eng_get_entry_points` |

## Testing

```sh
make test          # go test ./... — unit + integration suites
make test-mcp      # python pytest harness against a running daemon (fast)
make test-mcp-deep # add cross-validation against the live SQLite
```

`tests/mcp/` spawns `bin/veska-mcp` as a subprocess, drives every registered
tool with happy/bad/edge inputs, and pretty-prints each call's transcript so
the suite doubles as a human-readable smoke. Requires `VESKA_HOME` to point
at a running daemon's data dir and at least one `veska repo add`'d repo.

## Documentation

- [`docs/PRODUCT.md`](docs/PRODUCT.md) — what Veska is, in plain English.
- [`docs/README.md`](docs/README.md) — the design set, with a recommended read order.
- [`docs/design/`](docs/design/) — the `SOLO-NN` design sections and ADRs.
- [`docs/milestones/`](docs/milestones/) — milestone breakdowns (M0–M7 closed).
- [`docs/operations/`](docs/operations/) — config surface and runbooks.

## Status

Milestones **M0–M7 are complete**: substrate, identity & observability,
pipelines & embedder, the mechanical wiki, the optional review pipeline,
vuln + secrets scanning, and the design-set cutover. The build is green
(`make all`).
