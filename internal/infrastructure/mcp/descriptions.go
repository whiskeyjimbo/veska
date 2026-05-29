package mcp

// This file is the single source of truth for the user-facing descriptions
// of the MCP tools that also have a CLI wrapper (`veska calls`, `veska
// blast`, `veska search`). The cmd/veska package reuses these constants
// for the cobra Long help strings so that the warnings an MCP agent sees
// — chained_selectors_unresolved fallback, diff/dirty variants and
// cross-repo fan-out, RRF score range — can't drift from the CLI help.
// solov2-izh6.20.

// DescCallChain is the eng_get_call_chain MCP description and the
// `veska calls` Long help. It documents both degraded_reason fallbacks
// so junior CLI users learn about each gap without having to hit it first.
const DescCallChain = "Walk CALLS edges from a symbol. Use this — not search — when the question is 'what does this reach' (direction=out, default) or 'what calls this' (direction=in). Surfaces cross_repo_edges into other registered repos so library-symbol callers in a multi-repo workspace are visible without separate queries. Pass node_id (exact) or symbol (resolved via eng_find_symbol; ambiguity is rejected). NOTE: empty edges on a function/method seed carry one of two degraded_reasons hints: 'chained_selectors_unresolved' (parser limit — chained selector call sites like rootCmd.AddCommand(...).Execute() or s.field.M() are not yet modelled, epic solov2-9rc2) or 'external_callees_only' (index boundary — callees are stdlib or unregistered modules, NOT a parser bug). Fall back to eng_get_blast_radius, eng_search_semantic, or eng_find_symbol."

// DescBlastRadius is the eng_get_blast_radius MCP description and the
// `veska blast` Long help. Mentions the diff/dirty variants and the
// cross-repo fan-out behaviour.
const DescBlastRadius = "Compute the blast radius (callers/callees/both) of a symbol — 'if I change this, what breaks?' or 'what does this transitively reach?'. Use BEFORE editing an exported symbol, or when scoping a refactor. Walks cross_repo_edges in both directions so a library symbol's consumers in workspace repos are surfaced. Pass node_id (exact) or symbol (resolved via eng_find_symbol). For working-tree changes use eng_get_diff_blast_radius; for in-progress staged edits use eng_get_dirty_blast_radius."

// DescDirtyBlastRadius is the eng_get_dirty_blast_radius MCP description.
const DescDirtyBlastRadius = "Blast radius across every symbol currently in the staging overlay (mid-edit, pre-commit). Use during an active session to answer 'what am I about to break with my current edits?' without committing first. Unchanged-but-restaged symbols are filtered via content-hash compare so a comment-only edit doesn't dirty the whole file."

// DescDiffBlastRadius is the eng_get_diff_blast_radius MCP description.
const DescDiffBlastRadius = "Blast radius across every symbol in files changed in the working-tree vs HEAD. Use for PR review or 'what does this branch touch?' — the seed is the diff, not a single node."

// DescSearchSemantic is the eng_search_semantic MCP description and is
// embedded in the `veska search` Long help. Documents the RRF score range
// (~0.01–0.03) and that rank, not absolute score, is the right
// comparator across hits.
const DescSearchSemantic = "Natural-language search over embedded symbols (RRF-fused with FTS, lexical fallback when the embedder is offline). Best for behavior-shaped queries ('where do we validate session tokens'). Returns inline snippets so a follow-up Read is usually unnecessary. For known identifiers prefer eng_find_symbol (exact + deterministic); for 'what does this reach / who calls this' escalate to eng_get_call_chain / eng_get_blast_radius. With repo_id omitted (and cwd outside any registered repo) the query fans out across every registered repo in parallel and is fused with a single GLOBAL RRF so a top hit in one repo competes fairly with a top hit in another; each result then carries 'repo_id' so callers can disambiguate. The returned score is intra-query RRF (~0.01–0.03 typical range); use rank, not absolute score, to compare hits."
