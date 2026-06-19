# Veska Manual

**Veska** is a local code-intelligence daemon. It runs on your laptop, parses
your repository into a code graph (nodes + edges), embeds that graph
semantically, and serves both to your editor and your AI agent over MCP - so
they reason from the same structural ground truth instead of guessing.

This is the **operator and user manual**: how to install Veska, run the daemon,
connect your editor, and drive it day to day. For the design rationale and
architecture details, see the high-level architecture at `docs/ARCHITECTURE.md`.

## What it gives you

- **Grounded structural answers.** Every function, type, file, and call traces
  to a node, edge, or commit.
- **Semantic search.** Embeds your graph with an in-process embedder
  (model2vec by default - no external service), with a BM25 lexical fallback
  while indexing catches up.
- **Promotion checks.** On every commit, advisory findings flag dead code,
  contract drift, leaked secrets, and vulnerable `go.mod` dependencies.
- **Mechanical wiki.** Hot-zones and entry-points computed from the graph, no
  LLM in the path.

## One binary, three personalities

`make build` produces a single binary at `bin/veska`; `bin/veska-daemon` and
`bin/veska-mcp` are symlinks to it.

| Invocation | Role |
|---|---|
| `veska` | CLI - `init`, `repo`, `reindex`, `service`, `doctor`, `backup`, `wiki`, … |
| `veska-daemon` | Long-running process - owns the SQLite store, the file watcher, the embedder, and the post-promotion queue. |
| `veska-mcp` | Thin stdio shim proxying an editor's MCP connection to the daemon's Unix socket. |

## Where to go next

- New here? Start with **[Install](getting-started/install.md)** then the
  **[Quickstart](getting-started/quickstart.md)**.
- Want the mental model? Read **[Concepts](concepts/index.md)**.
- Running it for real? See the **[Operator guides](guides/running-the-daemon.md)**.
- Looking up a command, tool, or setting? Jump to the
  **[Reference](reference/cli.md)** (generated from the code).

!!! note "Everything lives in `~/.veska/`"
    Veska keeps its SQLite store, vector index, config, and logs under
    `~/.veska/` (override with `VESKA_HOME`). No server process, no external
    database.
