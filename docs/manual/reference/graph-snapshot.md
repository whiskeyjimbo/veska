# Graph snapshot (`veska graph export`)

`veska graph export` serializes a repo's code graph to a single deterministic
JSON file - the **graph snapshot**. The snapshot is a portable, shareable
projection of the graph: commit it so teammates can explore the graph without
indexing, or feed it to the read-only viewer (`veska graph serve`).

```bash
veska graph export graph-snapshot.json
veska graph export --repo myproj --branch main /tmp/snap.json
```

It opens the local graph database directly - **no running daemon is required**
(SQLite WAL reads coexist with a live daemon). The command resolves the repo
from `--repo`, else the current working directory, else the sole registered
repo, and exports that repo's active branch unless `--branch` overrides it.

| Flag | Default | Meaning |
|---|---|---|
| `--repo` | cwd repo, or the sole registered repo | Repo id, short_id, or alias to export. |
| `--branch` | the repo's registered active branch | Branch to export. |

## Guarantees

- **Deterministic.** Re-exporting an unchanged graph yields **byte-identical**
  output: nodes are emitted in graph-id order, edges sorted by edge id, and the
  hot-zone / entry-point / dependency lists inherit the deterministic order
  their producing services already guarantee. No wall-clock is embedded. This
  is what makes a committed snapshot diff cleanly.
- **No path leakage.** Every `file_path` / `path` is rewritten relative to the
  repo root, so no home directory or username appears in the file.
- **First-party only.** Vendored / module-cache (`external`) nodes are
  excluded; external code is represented through `dependencies`. Unresolved
  (proposed `SIMILAR_TO`) edges awaiting review are excluded; the snapshot
  carries confirmed structural relationships only.

## Schema (`schema_version: 1`)

The top-level object:

| Field | Type | Notes |
|---|---|---|
| `schema_version` | int | Snapshot format version. Bumped on any backward-incompatible change. |
| `repo_id` | string | The exported repo's id. |
| `branch` | string | The exported branch. |
| `nodes` | array | Code-graph nodes (see below). |
| `edges` | array | Confirmed structural edges (see below). |
| `hot_zones` | array | Files ranked by change risk, mirroring `docs/veska/hot_zones.md`. |
| `entry_points` | array | High-fan-in symbols, mirroring `docs/veska/entry_points.md`. |
| `dependencies` | array | External modules the repo calls into, mirroring `eng_list_dependencies`. |

### `nodes[]`

| Field | Type | Notes |
|---|---|---|
| `id` | string | Node id (deterministic content-derived hash). |
| `name` | string | Symbol name. |
| `kind` | string | `function`, `method`, `type`, `struct`, `interface`, `class`, `module`, `package`, `file`, `field`, `test`, `variable`, `command`, `route`, `chunk`. |
| `path` | string | Repo-root-relative source path. |
| `line_start`, `line_end` | int | 1-indexed line range (omitted when unknown). |
| `signature` | string | Function/method signature when present. |
| `language` | string | Source language. |
| `exported` | bool | Whether the symbol is exported (omitted when false). |
| `summary` | string | One-line summary: the stored `ShortSummary`, else the deterministic heuristic fallback (`<signature>`, else `<kind> <name>`). |
| `raw_content` | string | The node's source body, when stored (omitted for nodes without a snippet). |

### `edges[]`

| Field | Type | Notes |
|---|---|---|
| `id` | string | Edge id (deterministic from src+kind+tgt). |
| `src`, `tgt` | string | Endpoint node ids. |
| `kind` | string | `CALLS`, `IMPORTS`, `CONTAINS`, `TESTS`, `DEPENDS_ON`, `SIMILAR_TO`, `ROUTES`. |
| `confidence` | string | `probable`, `strong`, or `definite` (unresolved edges are excluded). |
| `resolved` | bool | Always true in the snapshot. |
| `source_line` | int | Call-site line when known. |
| `score` | float | Similarity score, present only for `SIMILAR_TO` edges. |

### `hot_zones[]`

`file_path` (repo-relative), `recent_change_frequency`, `blast_radius`, `score`.

### `entry_points[]`

`symbol_name`, `file_path` (repo-relative), `kind`, `inbound_count`,
`exported`, `has_adjacent_test`.

### `dependencies[]`

`module`, `version`, `language`, `usage_count`, `import_count`, and
`top_call_sites[]` (`src_node_id`, `symbol_path`).

## Viewing the snapshot (`veska graph serve`)

`veska graph serve` starts a read-only web viewer over localhost:

```bash
veska graph serve graph-snapshot.json   # serve a committed snapshot (no daemon)
veska graph serve                        # live in-process export of the current repo
veska graph serve --addr 127.0.0.1:9000  # pick the bind address
```

It renders the graph with [Cytoscape.js](https://js.cytoscape.org/) (vendored,
embedded in the binary - no network at view time). The initial view is scoped to
the entry points plus one representative node per hot-zone file; double-click a
node (or use the **Expand neighbors** button) to grow the visible subgraph, so a
10K+ node graph stays responsive. Nodes are colored by entry-point / hot-zone
classification; clicking one shows its `file:line`, summary, and full source.
The name search box filters the visible nodes and pulls matching symbols onto the
canvas. The server binds to localhost and exposes no write endpoints.

A live export ranks hot zones and entry points over the whole graph, so on a
large repo the server can take tens of seconds to bind; serving a committed
snapshot file is instant.

## Size

The snapshot embeds each node's source, so it scales with repo size (a ~13K-node
graph is ~15 MB). The viewer fetches it over HTTP rather than inlining it, so
large graphs are supported.
