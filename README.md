# Veska

**Veska** is a local code-intelligence daemon. It runs on your laptop, parses
your repository into a code graph (nodes + edges), embeds that graph
semantically, and serves both to your editor and your AI agent over MCP — so
they reason from the same structural ground truth instead of guessing.

## What it gives you

- **Grounded structural answers.** Every function, type, file, and call traces
  to a node, edge, or commit. Structural recall stays current within the
  save → staging freshness budget.
- **Eventually-consistent semantic search.** `semantic_search` embeds the graph
  with an in-process embedder (model2vec by default — no external service);
  during the indexing lag window it falls back to a BM25 lexical index and
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
| `veska` | CLI — `init`, `repo`, `reindex`, `service`, `doctor`, `backup`, `wiki`, … Run `veska --help` for the full list. |
| `veska-daemon` | Long-running process — owns the SQLite store, the fsnotify watcher, the embedder, and the post-promotion queue. Composition root: `cmd/veska-daemon/wire.go`. |
| `veska-mcp` | Thin stdio shim proxying an editor's MCP connection to the daemon's Unix socket. |

## Requirements

- **Go 1.26+**
- **No external services for core use.** SQLite, the vector index, and the
  default embedder all run in-process. A fresh machine indexes and searches
  with nothing else installed or running.

### Embedder

Semantic search needs an embedder. Veska **elects one at boot** in preference
order — it never mixes vector spaces, so exactly one embedder owns the index
at a time:

1. **model2vec** (`potion-code-16M`) — a fast, in-process static *code*
   embedder. The default and recommended choice. Get it either way:
   - **Fat binary** (`make build-fat`) — the model is compiled into the
     binary. Zero setup: nothing to install, no download, no network.
   - **Thin binary** (`make build`) + `veska install model2vec` — a one-time
     ~62 MB download into `~/.veska/`.
2. **static-v2** — an in-binary fallback that works with no model files at
   all (lower quality). Used only when model2vec is unavailable.

No Ollama, no network, and no separate process is required for search.

### Optional: Ollama

Ollama is **only** for the optional **LLM review pipeline** (off by default).
It is **not** used for embeddings in the default config. (Power users can force
an Ollama embedding model with `VESKA_EMBEDDER=ollama`, but model2vec is faster
and higher-quality on code, so this is rarely worthwhile.)

Install Ollama only if you want the review pipeline:

```sh
# macOS:        brew install ollama && ollama serve &
# Linux (snap): sudo snap install ollama && ollama serve &
# Linux (curl): curl -fsSL https://ollama.com/install.sh | sh && ollama serve &
```

## Build

```sh
make build        # thin: veska, veska-daemon, veska-mcp (+ layercheck tool)
make build-fat    # fat: same, but the model2vec model is embedded in the
                  #      binary — a zero-setup embedder (larger binaries)
make test         # go test ./...
make all          # build + test + vet + lint + layercheck
```

Binaries land in `./bin/`. Either `export PATH="$PWD/bin:$PATH"` or use the
`./bin/` prefix in the Quick Start below. Use `make build-fat` for the
zero-setup default embedder; plain `make build` keeps binaries small and
relies on `veska install model2vec` (or the static-v2 fallback).

## Quick start

```sh
# 1. Make sure an embedder is available (see Requirements):
#    - fat binary (make build-fat): nothing to do — the model is built in.
#    - thin binary: ./bin/veska install model2vec   (one-time ~62 MB)
#    With neither, search still works via the static-v2 fallback.

# 2. Initialise veska's data directory at ~/.veska/.
./bin/veska init

# 3. Start the daemon. For a quick try, background it:
./bin/veska-daemon &
# For a real install, run it as a real OS service (systemd --user on Linux,
# launchd on macOS):
./bin/veska service install
./bin/veska service start

# 4. Register a repo. The CLI dials the daemon's MCP socket so the cold
#    scan kicks off in the background. Tail ~/.veska/logs/daemon.log to
#    watch progress — every scan brackets a "cold scan: starting" and
#    "cold scan: complete" line.
./bin/veska repo add /path/to/your/repo

# 5. Sanity-check.
./bin/veska doctor status
```

The first `veska repo add` registers the repo, installs the git post-commit
hook with an absolute path to the `veska` binary, and dispatches a cold scan
through the daemon. Subsequent commits drive promotion via `eng_promote_repo`
on the daemon's MCP socket.

To force a re-scan of an already-registered repo (e.g. after a model swap):

```sh
./bin/veska reindex /path/to/your/repo
```

### Editor integration

Point your MCP client at `bin/veska-mcp` as a stdio command. Replace
`/abs/path/to/bin/veska-mcp` with the actual path on your machine.

**Claude Desktop** (`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS;
`%APPDATA%\Claude\claude_desktop_config.json` on Windows):

```json
{
  "mcpServers": {
    "veska": {
      "command": "/abs/path/to/bin/veska-mcp"
    }
  }
}
```

**Cursor** (`~/.cursor/mcp.json`) and **Zed** (`~/.config/zed/settings.json`,
under `context_servers`) accept the same `command` shape.

**Continue** (`~/.continue/config.yaml`):

```yaml
mcpServers:
  - name: veska
    command: /abs/path/to/bin/veska-mcp
```

If your `VESKA_HOME` is non-default, pass it through:

```json
{
  "mcpServers": {
    "veska": {
      "command": "/abs/path/to/bin/veska-mcp",
      "env": { "VESKA_HOME": "/path/to/veska/home" }
    }
  }
}
```

### Calling tools from the shell

Skip the editor and drive `veska-mcp` directly — handy for debugging or
scripting. The protocol is newline-delimited JSON-RPC; the method IS the
tool name (no `tools/call` envelope):

```sh
printf '{"jsonrpc":"2.0","id":1,"method":"eng_get_status","params":{}}\n' \
  | ./bin/veska-mcp \
  | jq .

printf '{"jsonrpc":"2.0","id":1,"method":"eng_find_symbol",
        "params":{"repo_id":"<id>","branch":"main","symbol":"Foo"}}\n' \
  | ./bin/veska-mcp | jq .result
```

Get `<id>` from `eng_list_repos`. The full tool surface is in
[MCP tools](#mcp-tools) below.

### Configuration

State lives under `~/.veska/` (`VESKA_HOME`). Daemon config is
`~/.veska/config.toml` — see [`docs/operations/CONFIG-SURFACE.md`](docs/operations/CONFIG-SURFACE.md).
Key environment variables:

| Var | Purpose | Default |
|---|---|---|
| `VESKA_HOME` | Data root | `~/.veska` |
| `VESKA_EMBEDDER` | Embedder election: `auto` (model2vec→static-v2), or force `model2vec` / `static` / `ollama` | `auto` |
| `VESKA_VECTOR_BACKEND` | `sqlite-vec` or `usearch` | `sqlite-vec` |
| `VESKA_OLLAMA_URL` | Ollama endpoint — review pipeline, and `VESKA_EMBEDDER=ollama` | `http://localhost:11434` |
| `VESKA_EMBED_MODEL` | Ollama embedding model — only when `VESKA_EMBEDDER=ollama` | `nomic-embed-text` |

The elected embedder is recorded in `~/.veska/embedder.locked`. Switching
embedders requires a re-index (`veska reindex`) since their vectors aren't
comparable.

## Architecture

```
cmd/                  the three binaries
internal/
  core/
    domain/           pure entities: Node, Edge, Graph, Task, Finding
    ports/            interface contracts (GraphStorage, VectorStorage, VulnSource, …)
  application/        use-case services: ingester, promoter, embedder, checks, review, wiki
  infrastructure/     adapters: sqlite, vector, embedding/{model2vec,static,ollama,elect}, treesitter, mcp, git
  repo/               repos-table registry
docs/                 design set (SOLO-NN sections), milestones, operations runbooks
```

## MCP tools

The daemon exposes 31 tools over a Unix-socket JSON-RPC server (forwarded to
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
| Findings | `eng_list_findings`, `eng_get_finding`, `eng_close_finding`, `eng_reopen_finding` |
| Suppressions | `eng_list_suppressions`, `eng_get_suppression`, `eng_suppress_finding`, `eng_close_suppression` |
| Wiki | `eng_get_hot_zone`, `eng_get_entry_points` |

**Conventions across the tool surface:**

- **Responses are `snake_case`.** Every tool emits the same node shape —
  `{node_id, name, kind, file_path, line_start, line_end, signature?,
  language?, exported?}` — plus `score`/`distance`/`snippet` on search and
  blast hits. Empty result collections serialize as `[]`, never omitted.
- **`repo_id` accepts a short alias.** `eng_list_repos` returns a 12-char
  `short_id` for each repo; anywhere a `repo_id` is required you may pass the
  full id or that short prefix. An unknown `repo_id` is a loud `NotFound`
  error, not an empty result.
- **Required params are reported together.** A call missing several required
  fields gets one error naming all of them.
- **Param names are canonical:** `file_path` (for node/file lookups, with
  `path` accepted as an alias), `node_id`, `symbol`. `eng_get_context_pack`
  takes exactly one of `symbol` or `task_id` (not `node_id`).
- **`eng_find_symbol` matches unqualified names:** searching `Start` finds
  `Server.Start`; exact matches rank first.
- **Embedder quality is in-band:** when the daemon is on the low-quality
  static-v2 fallback (no model2vec installed), every `eng_search_semantic`
  response carries `low_quality_static_embedder` in `degraded_reasons` — run
  `veska install model2vec` to clear it.

A task family (`eng_get_active_task`, `eng_set_active_task`,
`eng_get_task_history`) is implemented but currently **parked** — it is not
registered on the socket until a task backend (Jira / Linear / GitHub) lands to
populate the table (`wire.go`, solov2-6m1). Calling these returns
`method not found`.

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
