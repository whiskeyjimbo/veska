---
id: SOLO-12
title: "Wiki — Mechanical Pages and the Context Pack"
status: draft
version: 0.2.0
last_reviewed: 2026-05-16
verified: true
verified_date: "2026-05-16"
related: [SOLO-04, SOLO-08, SOLO-09, SOLO-11]
files:
  - internal/application/wiki/hotzone.go
  - internal/application/wiki/entrypoints.go
  - internal/application/wiki/render.go
  - internal/application/wiki/handler.go
  - internal/application/contextpack/contextpack.go
  - internal/infrastructure/mcp/tools_wiki.go
  - internal/infrastructure/mcp/tools_contextpack.go
  - internal/doctor/wiki_render.go
  - cmd/veska/wiki.go
---

# SOLO-12 — Wiki

The wiki is small on purpose: two mechanically derived page kinds
plus a small family of read-only MCP tools. Everything is computed
from the promoted graph and the commit history. No LLM is in the
loop. No synthesis prompts, no eval harness, no verifier model, no
fixture corpus.

## 1. The shape

As shipped in M4 (epic solov2-erp):

1. **`hot_zone`** — a generated Markdown page, one per repo, plus
   the `eng_get_hot_zone` MCP tool.
2. **`entry_points`** — a generated Markdown page, one per repo,
   plus the `eng_get_entry_points` MCP tool.
3. **`eng_get_context_pack`** — an MCP tool returning a
   token-bounded context bundle for a symbol or a task.

The Markdown page and the MCP tool for each kind are built from the
same in-memory report (`wiki.Report` / `wiki.EntryPointsReport`), so
the two surfaces never diverge.

## 2. `eng_get_context_pack`

A read-only MCP tool (`internal/application/contextpack`). The agent
calls it; the daemon returns JSON. No LLM in the path.

Input — exactly one of `symbol` or `task_id` is required:

```jsonc
{
  "symbol": "<exact symbol name>",   // symbol mode
  "task_id": "<TaskID>"              // task mode
}
```

The token budget is a server-side constructor option
(`contextpack.WithTokenBudget`, default `DefaultTokenBudget` = 8192);
it is not a per-call input field.

Output (`contextpack.Pack`):

```jsonc
{
  "repo_id": "...",
  "branch": "...",
  "mode": "symbol" | "task",
  "query": "<the symbol or task_id>",
  "nodes": [ /* relevant nodes: seeds + blast radius, BFS-distance ordered */ ],
  "recent_commits": [ /* commits touching those nodes' files, last 30 days */ ],
  "open_findings": [ /* {node_id} for relevant nodes carrying an open finding */ ],
  "tasks": [ /* the repo's active task, if any */ ],
  "estimated_tokens": 0,
  "token_budget": 8192,
  "truncated": false
}
```

How each section is derived:

- **`nodes`** — in symbol mode the seed set is `FindNodes(symbol)`;
  in task mode `domain.Task` carries no graph link, so the repo's
  working-tree diff (`ChangedFiles`) is treated as the seed set and
  relevant nodes are the nodes in those changed files (`NodesInFile`).
  Either seed set is expanded by `blastradius.Service`.
- **`recent_commits`** — `FileHistory` over the distinct files of the
  relevant nodes, 30-day window, capped at 25 files. Commit hash,
  author, time, subject. No LLM summary.
- **`open_findings`** — relevant nodes flagged by
  `FindingQuerier.OpenFindingNodeIDs`.
- **`tasks`** — the repo's active task, if any.

The bundle is clipped to the token budget by a deterministic
byte-length heuristic (`len(json)/4`). Lowest-priority sections are
dropped/clipped first — Tasks, then OpenFindings, then RecentCommits,
then Nodes — and `truncated` records whether anything was cut. An
oversized bundle is truncated, never rejected.

## 3. `hot_zone` page

One page per repo at `docs/veska/hot_zones.md`
(`wiki.HotZonesPagePath`). Mechanical derivation, in
`HotZoneService.Rank`:

1. For every file with commits in the look-back window, take its
   **recent change frequency** (commit count) from the git change
   counts.
2. For each such file, resolve the nodes it contains
   (`NodesInFile`) and run `blastradius.Service.Of` seeded with
   those nodes; the **blast radius** is the entry count of the
   result.
3. **Score = recent_change_frequency × blast_radius.**
4. Rank files by descending score; ties break by ascending file
   path. Retain the top N (`DefaultTopN` = 20, override with
   `WithTopN`).

`RenderHotZones` writes a Markdown table:
`Rank`, `File`, `Recent Changes`, `Blast Radius`, `Score`. No prose,
just the table and a one-line header.

## 4. `entry_points` page

One page per repo at `docs/veska/entry_points.md`
(`wiki.EntryPointsPagePath`). Mechanical derivation, in
`EntryPointsService.Select` — a symbol qualifies when **all three
gates** hold:

1. **Adjacent test.** It has an inbound edge from a node whose file
   path ends in `_test.go`.
2. **Low blast radius.** Its blast radius (`blastradius.Service`
   entry count) is at or below the configured maximum
   (`DefaultMaxBlastRadius` = 10, override with `WithMaxBlastRadius`).
3. **No open findings.** It carries no open finding.

Only symbol-bearing node kinds (function, method, type, struct,
interface, class) are candidates; files/packages/fields are
excluded. Entry points are ordered by ascending blast radius, then
ascending symbol name.

`RenderEntryPoints` writes a single Markdown table:
`Symbol`, `File`, `Kind`, `Blast Radius`. No LLM-written tour, no
narrative, no synthesis prose — that material would need an LLM in
the loop, which the wiki avoids.

## 5. Rendering

Pages are written to:

```
docs/veska/hot_zones.md
docs/veska/entry_points.md
```

The `docs/veska/` directory is created if absent. There is no
`INDEX.md` in the M4 surface.

`wiki.Handler` implements `ports.WorkHandler` for `WorkKindWiki`
rows. On each row it regenerates **both** pages and, only on full
success (both pages written and the stamp persisted), records the
wall-clock render time via the `RenderTimeStore` interface
(SQLite-backed `daemon_state` key `wiki.last_render_at`). Any error
— repo resolution, ranking, rendering, file write, stamp — propagates
wrapped so the `queue.Poller` retry path runs; a partial failure
leaves the previous stamp intact, and re-render is idempotent.

Regeneration runs at promotion time: a `WorkKindWiki` row is
enqueued on the post-promotion queue and drained in the same lane as
the embedding worker (SOLO-11).

The pages are deterministic: rendering a fixed promoted state twice
yields byte-identical output. The renderers iterate only over
already-sorted slices (no map-order leakage); the services sort and
tie-break every list they materialise.

### CLI and doctor

- **`veska wiki`** (`cmd/veska/wiki.go`) regenerates both pages on
  demand. It reuses the exact `wiki.Handler.Handle` orchestration the
  post-promotion lane runs, so CLI output is byte-identical to the
  queue-driven render. `--repo` / `--branch` flags select the target;
  an empty `--repo` defaults to the sole registered repo.
- **`veska doctor wiki_render`** (`internal/doctor/wiki_render.go`)
  reports the age of the last successful render. A never-rendered
  wiki and a freshly rendered one both report `healthy`; `broken` is
  reserved for a probe failure (nil store or query error).

The user can `git add docs/veska/` to commit the pages or add
`docs/veska/` to `.gitignore`. Veska does not stage them.

## 6. Performance

Budgets live in **SOLO-13 §3** under the canonical labelling
convention (`BUDGET (unmeasured)` / `(measured M<N>)` /
`INVARIANT` / `DEFAULT`). `eng_get_context_pack`, `hot_zone`
regeneration, and `entry_points` regeneration each have a row there.
If a budget misses we rewrite it in SOLO-13, not here.

## 7. What we deferred — M5/future, not shipped

Parked under `deferred/wiki/` (not in scope; return only behind
an ADR). **None of the following ships in M4 — they all require an
LLM in the loop, which the M4 wiki has none of:**

- `concept`, `entity`, `flow`, `decision`, `runbook`, `pitfall`
  page kinds (LLM-synthesised).
- Drift-lint and the re-synthesis loop.
- Persona rendering and persona profiles.
- Federation hooks (cross-repo wiki composition).
- Synthesis prompt set and per-kind JSON schemas.
- Eval harness, claim-extractor, verifier model, fixture corpus,
  exit criteria for synthesis quality.
- `wiki_render`, `wiki_get_page`, `wiki_search`,
  `wiki_list_drift` MCP tools.

Synthesis quality is hard to verify on a one-user single-laptop
product, and the verification machinery (eval harness, verifier
model) is itself a small product. We ship the mechanical layer
the synthesis layer would have consumed.
</content>
</invoke>
