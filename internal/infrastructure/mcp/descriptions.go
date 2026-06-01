package mcp

// This file is the single source of truth for the user-facing descriptions
// of the MCP tools that also have a CLI wrapper (`veska calls`, `veska
// blast`, `veska search`). The cmd/veska package reuses these constants
// for the cobra Long help strings so that the warnings an MCP agent sees
// — chained_selectors_unresolved fallback, diff/dirty variants and
// cross-repo fan-out, RRF score range — can't drift from the CLI help.
// solov2-izh6.20.
//
// Two scopes of constant live here. The whole-description constants
// (DescCallChain, DescBlastRadius, DescSearchSemantic) cover query tools
// whose MCP params (node_id/symbol/repo) map cleanly onto the CLI flags, so
// embedding the full string in the CLI Long stays accurate. The
// invariant-fragment constants below (DescDepsImportOnlyCaveat,
// DescFindSymbolMatching, DescContextPack) cover tools whose MCP JSON
// surface is WIDER than the CLI wrapper — `eng_get_context_pack` accepts
// node_id/symbol/task_id but `veska context` takes only a symbol;
// `eng_list_dependencies` returns a top_call_sites JSON shape the CLI table
// doesn't. For those, only the drift-risk fact that is true on BOTH
// surfaces is shared; the MCP-only parameter/shape prose is composed in at
// the MCP registration site so the CLI help never claims an input the
// command rejects.

// DescCallChain is the eng_get_call_chain MCP description and the
// `veska calls` Long help. It documents both degraded_reason fallbacks
// so junior CLI users learn about each gap without having to hit it first.
// The chained-selector parser limit is tracked by epic solov2-9rc2.
const DescCallChain = "Walk CALLS edges from a symbol. Use this — not search — when the question is 'what does this reach' (direction=out, default) or 'what calls this' (direction=in). Surfaces cross_repo_edges into other registered repos so library-symbol callers in a multi-repo workspace are visible without separate queries. Pass node_id (exact) or symbol (resolved via eng_find_symbol; ambiguity is rejected). NOTE: empty edges on a function/method seed carry one of two degraded_reasons hints: 'chained_selectors_unresolved' (parser limit — chained selector call sites like rootCmd.AddCommand(...).Execute() or s.field.M() are not yet modelled) or 'external_callees_only' (index boundary — callees are stdlib or unregistered modules, NOT a parser bug). Fall back to eng_get_blast_radius, eng_search_semantic, or eng_find_symbol."

// DescBlastRadius is the eng_get_blast_radius MCP description and the
// `veska blast` Long help. Mentions the diff/dirty variants and the
// cross-repo fan-out behaviour.
const DescBlastRadius = "Compute the blast radius (callers/callees/both) of a symbol — 'if I change this, what breaks?' or 'what does this transitively reach?'. Use BEFORE editing an exported symbol, or when scoping a refactor. Walks cross_repo_edges in both directions so a library symbol's consumers in workspace repos are surfaced. Pass node_id (exact) or symbol (resolved via eng_find_symbol). For working-tree changes use eng_get_diff_blast_radius; for in-progress staged edits use eng_get_dirty_blast_radius."

// DescDirtyBlastRadius is the eng_get_dirty_blast_radius MCP description.
const DescDirtyBlastRadius = "Blast radius across every symbol currently in the staging overlay (mid-edit, pre-commit). Use during an active session to answer 'what am I about to break with my current edits?' without committing first. Unchanged-but-restaged symbols are filtered via content-hash compare so a comment-only edit doesn't dirty the whole file."

// DescDiffBlastRadius is the eng_get_diff_blast_radius MCP description.
const DescDiffBlastRadius = "Blast radius across every symbol in files changed by a git diff. By default the diff is the working tree vs HEAD; supply ref_a and ref_b together to blast a ref range (e.g. main..HEAD) instead. Use for PR review or 'what does this branch touch?' — the seed is the diff, not a single node."

// DescSearchSemantic is the eng_search_semantic MCP description and is
// embedded in the `veska search` Long help. Documents the RRF score range
// (~0.01–0.03) and that rank, not absolute score, is the right
// comparator across hits.
const DescSearchSemantic = "Natural-language search over embedded symbols (RRF-fused with FTS, lexical fallback when the embedder is offline). Best for behavior-shaped queries ('where do we validate session tokens'). Returns inline snippets so a follow-up Read is usually unnecessary. For known identifiers prefer eng_find_symbol (exact + deterministic); for 'what does this reach / who calls this' escalate to eng_get_call_chain / eng_get_blast_radius. With repo_id omitted (and cwd outside any registered repo) the query fans out across every registered repo in parallel and is fused with a single GLOBAL RRF so a top hit in one repo competes fairly with a top hit in another; each result then carries 'repo_id' so callers can disambiguate. The returned score is intra-query RRF (~0.01–0.03 typical range); use rank, not absolute score, to compare hits."

// DescDepsImportOnlyCaveat is the invariant fragment shared by the
// eng_list_dependencies MCP description and the `veska deps list` Long help:
// the dependency list is sourced from resolved package-qualified call sites,
// so a module that is imported but only referenced indirectly is absent. The
// MCP registration composes the cross_repo_edge_stubs/top_call_sites prose
// around this; the CLI table output doesn't expose those, so only the caveat
// is shared.
const DescDepsImportOnlyCaveat = "a module imported but only referenced via struct literals / type assertions (no resolved package-qualified call) will not appear yet"

// DescFindSymbolMatching is the invariant fragment shared by the
// eng_find_symbol MCP description and the `veska symbol` Long help: the
// unqualified-name matching rule and exact-first ordering. The MCP-only
// node_id-chaining prose (feed into eng_get_call_chain etc.) is composed in
// at the registration site, since the CLI surfaces those as separate
// `veska` subcommands, not chained node_ids.
const DescFindSymbolMatching = "Unqualified names also match — 'Run' finds Server.Run, Command.Run, etc., with exact matches first."

// DescContextPack is the shared purpose-and-behaviour description for
// eng_get_context_pack and the `veska context` Long help. It covers what the
// bundle contains and the cross-repo fan-out — both true on either surface.
// The MCP-only anchor-selection prose ("pass exactly one of
// node_id/symbol/task_id") is composed in at the registration site because
// the CLI wrapper accepts only a symbol.
const DescContextPack = "Bundle a symbol's neighbourhood (callers, callees, adjacent tests, recent commits, open findings, active task) into one token-bounded payload. Use at the START of a non-trivial change so you don't have to assemble surrounding context piecewise. Surfaces cross_repo_edges in both directions, so cross-repo callers/callees show up in the same response."
