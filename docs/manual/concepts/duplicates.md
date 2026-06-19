# Finding duplicate & similar code

Veska clusters similar code across your repo (or across every registered repo)
so you can triage it for de-duplication. It is a query over the same graph
everything else uses - no separate scan. One command returns the groups,
ranked, each shaped so you can open a verify-and-dedupe task per cluster.

```bash
veska duplicates                 # one repo: ranked clusters, all tiers
veska duplicates --all-repos     # across every registered repo
veska duplicates --tiers structural --path internal/ --json
```

The same view is available to your agent over MCP as `eng_find_clusters`
(no seed required).

## Three tiers

A cluster is a group of **two or more** symbols that resemble each other. Every
cluster carries a **tier** that says *how* they resemble each other, tightest
first. A symbol appears at most once, at its tightest tier.

- **exact** - byte-for-byte identical source (literal copy-paste), by
  `content_hash` equality. Deterministic, no embeddings.
- **structural** - the same **shape** after a consistent rename of variables,
  parameters, and literals (Type-2 clones). Two functions with identical bodies
  but different names - or `a + b` vs `x + y` - land here even though their text
  differs. Computed from an identifier-normalized hash of the syntax tree at
  parse time.
- **near** - **semantically** similar above the elected embedder's calibrated
  threshold (the `SIMILAR_TO` edges auto-link already stores). The loosest tier:
  renamed-and-drifted copies, parallel implementations. Threshold-sensitive -
  lower `--min-score` for more recall, accept more noise.

!!! tip "Which tier should I act on first?"
    `exact` and `structural` are high-precision de-dupe candidates - the code is
    genuinely the same, just copied or renamed. `near` is for *review*: it casts
    a wider net and benefits from a human (or agent) deciding whether the
    similarity is worth consolidating.

## Scope

- **One repo** (default) - clusters within the repo you point at (`--repo`, or
  the repo resolved from your cwd).
- **Cross-repo** (`--all-repos`) - clusters across **every** registered repo, so
  a function copy-pasted from one project into another shows up as one cluster
  whose members span both. Cross-repo covers the **exact** and **structural**
  tiers; the **near** tier stays within a single repo for now.
- **Path** (`--path internal/infrastructure/mcp`) - restrict the sweep to a
  file-path prefix for a focused pass.

Container and sub-symbol kinds (package, file, module, field, import, and the
synthetic chunk nodes) are excluded so boilerplate doesn't flood the results.

## Before the structural & near tiers work

The structural and near tiers read data populated at **parse/promotion time**:
the `structural_hash` on each node, and the similarity score on each
`SIMILAR_TO` edge. A repository indexed before those landed carries neither
until you **re-index** it:

```bash
veska reindex <repo>
```

After a re-index the `exact` tier is unchanged, and `structural` / `near` start
returning clusters. The `exact` tier needs nothing special - it works on any
promoted graph.

## Related

- For *"what else looks like **this one** symbol?"* use
  [`veska similar <symbol>`](../reference/cli.md) (`eng_search_similar`) - the
  per-symbol pivot, no group-wide scan.
- `veska clones` is the older, single-tier view (exact, or `--near`); `veska
  duplicates` supersedes it with the unified, ranked, cross-repo output.
- See [The code graph](graph.md) for how nodes and edges are built, and
  [Semantic search & embeddings](embeddings.md) for what powers the near tier.
