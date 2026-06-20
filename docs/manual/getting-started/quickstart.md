# Quickstart

This walks you from a built binary to a daemon serving your repo, in five
steps. Commands assume `./bin/` - drop the prefix if Veska is on your `PATH`.

## 1. Initialize the data directory

```sh
./bin/veska init
```

Creates `~/.veska/` (override with `VESKA_HOME`) and writes
`~/.veska/config.toml`, including an active `[vuln_source]` block so promotion
checks run by default. Opt out with `veska init --no-vuln`.

## 2. Start the daemon

For kicking the tires, background it:

```sh
./bin/veska-daemon &
```

For a real install, run it as an OS service (systemd `--user` on Linux, launchd
on macOS) - auto-restart on crash, logs under `~/.veska/logs/`:

```sh
./bin/veska service install
./bin/veska service start
```

See **[Running the daemon](../guides/running-the-daemon.md)** for the full
service lifecycle.

## 3. Register a repo

```sh
./bin/veska repo add /path/to/your/repo --wait
```

`--wait` blocks until the cold scan finishes (a few seconds for most repos) so
your first search is already hot. Without it, the scan runs in the background
and an early `eng_search_semantic` may return `[]` with
`degraded_reasons=embeddings_pending` until indexing catches up - tail
`~/.veska/logs/daemon.log` for the `cold scan: complete` line.

`repo add` also installs the git post-commit hook (absolute path to the `veska`
binary). Subsequent commits drive promotion automatically.

## 4. Sanity-check

```sh
./bin/veska doctor status
```

See **[Diagnostics with doctor](../guides/doctor.md)** for what the output
means.

## 5. First MCP call - 60-second check

Once `cold scan: complete` shows in the log, drive two MCP tools from the shell
before pointing an editor at the daemon:

```sh
# Find a symbol by name (exact matches ranked first).
printf '{"jsonrpc":"2.0","id":1,"method":"eng_find_symbol","params":{"symbol":"Run"}}\n' \
  | ./bin/veska-mcp | jq '.result.nodes[0]'

# Natural-language search; results carry inline snippets.
printf '{"jsonrpc":"2.0","id":1,"method":"eng_search_semantic","params":{"query":"parse config"}}\n' \
  | ./bin/veska-mcp | jq '.result.results[:3]'
```

A response with `file_path`, `line_start/line_end`, and `name` means the daemon
is parsing and serving your repo correctly.

## Re-scanning

To force a re-scan of an already-registered repo (e.g. after a model swap):

```sh
./bin/veska reindex /path/to/your/repo
```

Safe to run while the daemon is up - it dispatches through the daemon's
`eng_reindex_repo` tool, so your editor's MCP connection is not interrupted.

## Next

- **[Connect your editor](../guides/editor-setup.md)** - wire `veska-mcp` into
  Claude Desktop, Cursor, or any MCP client.
- **[Concepts](../concepts/index.md)** - the mental model behind the graph,
  promotion, and search.
