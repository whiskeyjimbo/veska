# Daemon topology

Veska ships as a **single Go binary** that behaves as three things depending on
how it's invoked. `make build` produces `bin/veska`; `bin/veska-daemon` and
`bin/veska-mcp` are **symlinks to the same binary**, and an `argv[0]` dispatcher
routes each into its own code path.

| Invocation | Role |
|---|---|
| `veska` | **CLI** — `init`, `repo`, `reindex`, `service`, `doctor`, `backup`, `wiki`, … Operator and user surface. |
| `veska-daemon` | **Long-running process** — owns the SQLite store, the fsnotify watcher, the embedder, the MCP Unix-socket server, and the post-promotion queue poller. |
| `veska-mcp` | **Thin stdio shim** — proxies an editor's MCP (JSON-RPC) frames to the daemon's Unix socket. What your editor launches. |

## What the daemon owns

The daemon is the single writer. Everything Veska knows lives under `~/.veska/`
(override with `VESKA_HOME`):

- **SQLite store** — the durable graph, the post-promotion queue, and the FTS
  (lexical) index. In-process, no server.
- **Vector index** — in-memory by default (`memvec`); an on-disk HNSW backend
  (usearch) is available for large graphs via `VESKA_VECTOR_BACKEND=usearch`.
- **File watcher** — fsnotify, debounced, driving save → staging reparses.
- **Embedder** — in-process model2vec by default.
- **MCP server** — a Unix socket the `veska-mcp` shim and CLI dispatch through.
- **Post-promotion queue poller** — drains embedding/check/autolink work after
  each commit.

## How the pieces talk

```
editor  ──stdio JSON-RPC──▶  veska-mcp  ──Unix socket──▶  veska-daemon
                                                              │
  veska CLI  ──Unix socket──────────────────────────────────┤  owns ~/.veska/
                                                              │  (SQLite, vectors,
  git post-commit hook  ──▶  veska  ──promote──▶  daemon      │   watcher, embedder)
```

Most CLI commands (`reindex`, `repo add`, search) **dispatch through the
daemon** when it's running, so they never interrupt your editor's MCP
connection. With the daemon stopped, some fall back to a direct in-process path.

## Running it

For real use, run the daemon as an OS service (systemd `--user` on Linux,
launchd on macOS) so it starts on boot, auto-restarts on crash, and logs under
`~/.veska/logs/`. See **[Running the daemon](../guides/running-the-daemon.md)**.
