---
id: SOLO-12
title: "Wiki — Mechanical Pages and the Context Pack"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-04, SOLO-08, SOLO-09, SOLO-11]
---

# SOLO-12 — Wiki

The wiki is small on purpose: two mechanically derived page kinds
and one MCP tool. Everything is computed from the promoted graph
and the commit history. No LLM is in the loop. No synthesis
prompts, no eval harness, no verifier model, no fixture corpus.

## 1. The shape

Three artifacts:

1. **`eng_get_context_pack`** — an MCP tool returning task-scoped
   context for the active task.
2. **`hot_zone`** — a generated Markdown page, one per repo.
3. **`entry_points`** — a generated Markdown page, one per repo.

That is the entire wiki product.

## 2. `eng_get_context_pack`

A read-only MCP tool. The agent calls it; the daemon returns
JSON. No LLM in the path.

Input:

```jsonc
{
  "task_id": "<TaskID, optional; default = active task>",
  "max_tokens": 8000
}
```

Output (summary projection per SOLO-09 §4.1):

```jsonc
{
  "task": { "id": "...", "title": "..." },
  "blast_radius": [ /* nodes within 2 hops of the task's touched set */ ],
  "recent_commits": [ /* commits in the last 14 days touching those files */ ],
  "open_findings":  [ /* findings whose target is in the blast radius */ ],
  "central_symbols": [ /* top-degree nodes in the blast radius */ ],
  "included_staging": true
}
```

What goes into each field:

- **`blast_radius`** — `eng_get_blast_radius` over the task's
  touched nodes (i.e. nodes that appear in `task_touches` rows
  while this task is active), depth 2, exclude unresolved.
- **`recent_commits`** — Git log, last 14 days, intersected with
  the file set of `blast_radius`. No LLM summary; commit message
  + sha + author + date.
- **`open_findings`** — `findings` rows where
  `status == open` and the target node is in the blast radius.
- **`central_symbols`** — top 20 nodes in the blast radius
  ranked by in-degree on `CALLS ∪ IMPORTS` edges (promoted only).

The whole call is bounded by `max_tokens` (per SOLO-09 §4.3); the
envelope sets `truncated_at: N` when the budget bites.

## 3. `hot_zone` page

One page per repo at `docs/engram/hot_zone.md`. Mechanical
derivation:

1. Compute centrality on the promoted graph: PageRank over
   `CALLS ∪ IMPORTS`, top 50 nodes.
2. Compute churn: count commits in the last 90 days touching
   each file in the repo, take the top 50 files.
3. Intersect: nodes that appear in both the top-50 centrality
   and the top-50 churn.
4. Render a Markdown table: `node`, `path`, `centrality_rank`,
   `churn_count`, `last_touched`.

No prose. No "we think this matters because…". Just the table
and a one-line header that says when it was generated.

## 4. `entry_points` page

One page per repo at `docs/engram/entry_points.md`. Mechanical
derivation:

1. **Public symbols.** Public-API functions per the language's
   visibility rules (Go: `[A-Z]…` exported; TS/JS: exported
   declarations).
2. **Entry points.** `main`, `init`, top-level handler
   registrations, CLI command definitions.
3. **Most-tested symbols.** Nodes with the most inbound `TESTS`
   edges.
4. Take the top 10 of each. Render three sections, each a small
   table: `name`, `path`, `signature`, `lines`.

The page intends to give a new contributor a starting set of
symbols to read. No LLM-written tour, no narrative, no synthesis
prose — that material would need an LLM in the loop, which the
wiki avoids.

## 5. Rendering

Pages are written to:

```
docs/engram/INDEX.md
docs/engram/hot_zone.md
docs/engram/entry_points.md
```

`INDEX.md` is a 5-line file linking the two pages and their
generation timestamp. The directory is created if absent.

Generation runs at promotion time, in the same post-promotion queue drain as the
embedding worker (SOLO-11). Each page is regenerated whenever
the promotion touched any file or symbol that affects its inputs:

- `hot_zone` regenerates if any file in the top-90-day churn set
  changed.
- `entry_points` regenerates if any public symbol or entry point
  changed.

Regeneration is idempotent **with explicit determinism
enforcement**: byte-identical output across runs requires the
renderer to sort and tie-break every list it materialises. The
rules:

1. PageRank ties (equal centrality_rank) break by ascending
   `(repo_id, branch, symbol_path)`.
2. Churn ties (equal churn_count) break by ascending file path.
3. The intersection in `hot_zone` is iterated in
   `(centrality_rank ASC, symbol_path ASC)` order.
4. `entry_points` sections order by `(signature ASC, path ASC)`
   inside each section.
5. The generation timestamp is rendered with second precision
   in UTC; sub-second jitter is not part of the output.

The renderer goes through a `tools/lint/wikidet` analyser at
test time that re-runs `veska wiki regenerate` twice in a row
and diffs the output bytewise. CI fails on any difference. The
user can `git add docs/engram/` to commit the pages or add
`docs/engram/` to `.gitignore`. Engram does not stage them.

## 6. Performance

Budgets live in **SOLO-13 §3** under the canonical labelling
convention (`BUDGET (unmeasured)` / `(measured M<N>)` /
`INVARIANT` / `DEFAULT`). `eng_get_context_pack`, `hot_zone`
regeneration, and `entry_points` regeneration each have a row there.
M1 closes the spike; if a budget misses we rewrite it in SOLO-13,
not here.

## 7. What we deferred

Parked under `deferred/wiki/` (not in scope; return only behind
an ADR):

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
