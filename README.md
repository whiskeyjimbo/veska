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
  `Finding`s: dead code, contract drift, leaked secrets, and — opt-in —
  vulnerable `go.mod` dependencies via the OSV.dev advisory database. The
  vuln check is gated behind a `[vuln_source]` config block in
  `~/.veska/config.toml` (see
  [`docs/operations/CONFIG-SURFACE.md`](docs/operations/CONFIG-SURFACE.md));
  the other three ship on by default. **Lifecycle:** the block is read at
  daemon start, so add it before `veska service start` (or restart the
  service after editing). New scans pick it up automatically; to scan
  already-promoted repos retroactively, run `veska reindex <path>`.
- **Optional LLM review.** An off-by-default post-promotion review pipeline.
- **Mechanical wiki.** Hot-zones and entry-points computed from the graph,
  no LLM in the path. The `eng_get_hot_zone` and `eng_get_entry_points`
  MCP tools return data in-memory and write nothing; the `veska wiki`
  CLI renders the same data into `docs/veska/{hot_zones,entry_points}.md`
  inside the repo (re-runnable, idempotent — bracket markers in each
  page preserve any hand edits outside the managed block). A context-pack
  tool sits alongside.
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
   - **Fat binary** (`make build`, default) — the model is compiled into the
     binary. Zero setup: nothing to install, no download, no network.
   - **Thin binary** (`make build-small`) + `veska install model2vec` — a
     one-time ~62 MB download into `~/.veska/`.
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

`make build` is the fat binary by default (solov2-sft7) — it embeds the
model2vec weights into the binary so the install is zero-setup: no separate
download, no network, no static-v2 fallback at boot.

```sh
make build        # default: ~104 MB fat binary (model2vec ~62 MB embedded
                  # into a ~42 MB thin build). Zero setup at runtime.
make build-small  # ~42 MB thin: veska, veska-daemon, veska-mcp (+ layercheck).
                  # Use this only when you want size-sensitive binaries
                  # (CI, containers); you must then run `veska install model2vec`
                  # to avoid booting on the low-quality static-v2 fallback.
make test         # go test ./...
make all          # build-small + test + vet + lint + layercheck
                  # (uses the thin build to keep the test loop fast)
```

Binaries land in `./bin/`. Either `export PATH="$PWD/bin:$PATH"` or use the
`./bin/` prefix in the Quick Start below.

### Install into your `PATH`

After a `make build`, drop the binaries into a user bin directory in one step:

```sh
make install                         # → ~/.local/bin (default)
VESKA_INSTALL_DIR=/usr/local/bin sudo make install   # system-wide
```

For a self-contained tarball (the three fat binaries + `install.sh` + a
README), run `make release-archive`. The archive at
`dist/veska-<version>-<os>-<arch>.tar.gz` is the same shape a future
GitHub release will ship — `./install.sh` from inside the extracted
directory does the same thing as `make install` (solov2-cdw3).

## Quick start

```sh
# 1. Build veska (default: fat, zero-setup embedder).
make build
# Size-sensitive builds can `make build-small` instead, then run
# `./bin/veska install model2vec` to avoid the low-quality static-v2 fallback.

# 2. Initialise veska's data directory at ~/.veska/.
./bin/veska init

# 3. Start the daemon.
#
#    Pick one:
#      - Just kicking the tyres? Background it:    ./bin/veska-daemon &
#      - Want it on every boot, auto-restart on
#        crash, logs under ~/.veska/logs?           use the service form below.
#
#    For a real install, run it as an OS service (systemd --user on Linux,
#    launchd on macOS). Uninstall with `./bin/veska service uninstall`.
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

Safe to run while the daemon is up — the CLI dispatches the cold-scan
through the daemon's `eng_reindex_repo` MCP tool (solov2-4d7b), so your
editor's MCP connection is not interrupted. With the daemon stopped, the
same command falls back to a direct in-process reparse.

### First call — 60 second sanity check

Once `cold scan: complete` shows in `~/.veska/logs/daemon.log`, drive two
MCP tools from the shell so you've seen real output before pointing an
editor at the daemon:

```sh
# Find a symbol by name. Unqualified matches are fine — "Run" finds
# Server.Run, Command.Run, etc., with exact matches ranked first.
printf '{"jsonrpc":"2.0","id":1,"method":"eng_find_symbol","params":{"symbol":"Run"}}\n' \
  | ./bin/veska-mcp | jq '.result.nodes[0]'

# Natural-language search; results carry inline snippets so a follow-up
# Read is usually unnecessary.
printf '{"jsonrpc":"2.0","id":1,"method":"eng_search_semantic","params":{"query":"parse config"}}\n' \
  | ./bin/veska-mcp | jq '.result.results[:3]'
```

Either tool's response should contain `file_path`, `line_start/line_end`,
and a `name` — if you see those, the daemon is parsing and serving your
repo correctly. `eng_search_semantic` may return `[]` on a freshly
registered repo while embeddings finish populating; check
`eng_get_status`'s `pending_embeds` count.

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

### Per-agent instruction snippets

`veska init --agent <name>` writes (or safely appends to) an agent-specific
instruction file inside the current project so the agent knows the Veska tool
surface is available. Supported agents:
`claude`, `codex`, `copilot`, `cursor`, `gemini`, `kiro`, `opencode`.

```sh
cd /path/to/your/repo
./bin/veska init --agent claude    # creates or updates CLAUDE.md
```

The snippet is bracketed with `<!-- veska:init -->` markers so re-running the
command updates only the Veska section and leaves the rest of the file alone.

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
| Context | `eng_get_context_pack`, `eng_find_changed_symbols` (takes `ref_a`/`ref_b` or aliases `base`/`head`; defaults to `HEAD~1..HEAD`; chunks filtered, comment-only diffs surface `non_symbol_changes_only` in `degraded_reasons`) |
| Misc | `eng_find_owner`, `eng_find_todos` |
| Findings | `eng_list_findings`, `eng_get_finding`, `eng_close_finding`, `eng_reopen_finding` |
| Suppressions | `eng_list_suppressions`, `eng_get_suppression`, `eng_suppress_finding`, `eng_close_suppression` |
| Wiki | `eng_get_hot_zone`, `eng_get_entry_points` |
<!-- Parked task family (eng_get_active_task, eng_set_active_task, eng_get_task_history)
     is intentionally omitted from this table — it is unregistered and would
     return `method not found` if called. See "Parked tools" note further below. -->


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
- **Param names are canonical, with journey-natural aliases:** `file_path`
  (alias `path` on `eng_find_owner` / `eng_get_file_nodes`), `node_id`,
  `symbol`. `eng_find_changed_symbols` accepts `base`/`head` alongside
  `ref_a`/`ref_b`; `eng_search_similar` accepts `symbol` alongside `node_id`
  (resolved via `FindNodes` with the same ambiguity-rejection as
  `eng_get_blast_radius`). `eng_get_context_pack` takes exactly one of
  `node_id`, `symbol`, or `task_id`.
- **Cross-repo edges are uniform.** `eng_get_call_chain`,
  `eng_get_blast_radius`, and `eng_get_context_pack` all surface a
  `cross_repo_edges` array when nodes in the response have CALLS edges
  pointing into another registered repo. Both directions are walked: a
  blast/call_chain in the callers direction (or context_pack's both)
  finds consumers of a library symbol in other repos via the reverse
  stub resolver.
- **`eng_find_symbol` matches unqualified names:** searching `Start` finds
  `Server.Start`; exact matches rank first.
- **Embedder quality is in-band:** when the daemon is on the low-quality
  static-v2 fallback (no model2vec installed), every `eng_search_semantic`
  response carries `low_quality_static_embedder` in `degraded_reasons` — run
  `veska install model2vec` to clear it.

**Parked tools.** A task family (`eng_get_active_task`,
`eng_set_active_task`, `eng_get_task_history`) is implemented in the tree
but **not** registered on the socket — calling it returns `method not
found`. It stays parked until a task backend (Jira / Linear / GitHub) lands
to populate the underlying table (`wire.go`, solov2-6m1). Treat it as
non-existent for now; agent instruction snippets do not advertise it.

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
