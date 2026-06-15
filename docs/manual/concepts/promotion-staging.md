# Promotion & staging

Veska's graph runs on **two clocks**. Understanding them explains why some
answers are instant and others "catch up."

## Save → staging (structural recall)

When you save a file, the daemon's file watcher (fsnotify) debounces the event
and tree-sitter reparses the changed file. The new parse lands in an **in-memory
staging overlay** — so structural questions ("what symbols are in this file
now?", "what calls this function?") reflect your uncommitted edits within the
**save → staging freshness budget**: the save event, the debounce, and the
reparse.

This is fast — typically sub-second on a quiet laptop with small files. It is a
**measured budget, not a hard real-time guarantee**: macOS coalesces FSEvents,
and large files are dominated by parse cost.

!!! warning "Staging is volatile"
    The staging overlay is in-memory. A daemon crash before you commit drops
    unpromoted parses — the next save reproduces them. Nothing durable is lost
    because your source is the source of truth.

## Commit → promotion (durable graph)

When you **commit**, the git post-commit hook (installed by `veska repo add`)
drives **promotion**: the staged parse for that commit is written durably into
the SQLite graph, atomically. Promotion is the moment the graph for that commit
becomes the persisted ground truth.

Promotion also kicks off the **post-promotion queue**: the work that happens
*after* the commit lands — embedding the changed symbols, running advisory
checks, auto-linking. The daemon drains this queue in the background.

### Promotion checks

Each promotion runs synchronous advisory checks that emit `Finding`s — dead
code, contract drift, leaked secrets, and vulnerable `go.mod` dependencies (via
the OSV.dev database). They are **advisory**: they inform, they don't block your
commit. See **[Diagnostics with doctor](../guides/doctor.md)** and the
`eng_list_findings` tool.

## The two clocks, side by side

| | Structural recall | Semantic recall |
|---|---|---|
| Triggered by | save | commit (promotion) |
| Latency | save → staging budget (sub-second typical) | queue drain (seconds, default embedder) |
| Storage | in-memory staging | durable SQLite + vector index |
| Guarantee | always current | eventually consistent |

The promise: **structural recall is always current; semantic recall catches up
quickly.** The next page explains the semantic side.

Next: **[Semantic search & embeddings](embeddings.md)**.
