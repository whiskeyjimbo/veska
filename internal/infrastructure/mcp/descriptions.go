// SPDX-License-Identifier: AGPL-3.0-only

package mcp

// This file provides shared user-facing descriptions for MCP tools and CLI commands to prevent documentation drift.
// Constants are split into full descriptions (where MCP and CLI parameters map 1:1) and invariant fragments
// (where the MCP surface accepts more inputs than the CLI command).

const DescCallChain = "Walk CALLS edges from a symbol. Use this - not search - when the question is 'what does this reach' (direction=out, default) or 'what calls this' (direction=in). Surfaces cross_repo_edges into other registered repos so library-symbol callers in a multi-repo workspace are visible without separate queries. Pass node_id (exact) or symbol (resolved via eng_find_symbol; ambiguity is rejected). NOTE: empty edges on a function/method seed carry one of two degraded_reasons hints: 'chained_selectors_unresolved' (parser limit - chained selector call sites like rootCmd.AddCommand(...).Execute() or s.field.M() are not yet modeled) or 'external_callees_only' (index boundary - callees are stdlib or unregistered modules, NOT a parser bug). Fall back to eng_get_blast_radius, eng_search_semantic, or eng_find_symbol."

const DescBlastRadius = "Compute the blast radius (callers/callees/both) of a symbol - 'if I change this, what breaks?' or 'what does this transitively reach?'. Use BEFORE editing an exported symbol, or when scoping a refactor. Walks cross_repo_edges in both directions so a library symbol's consumers in workspace repos are surfaced. Pass node_id (exact) or symbol (resolved via eng_find_symbol). For working-tree changes use eng_get_diff_blast_radius; for in-progress staged edits use eng_get_dirty_blast_radius."

const DescDirtyBlastRadius = "Blast radius across every symbol currently in the staging overlay (mid-edit, pre-commit). Use during an active session to answer 'what am I about to break with my current edits?' without committing first. Unchanged-but-restaged symbols are filtered via content-hash compare so a comment-only edit doesn't dirty the whole file."

const DescDiffBlastRadius = "Blast radius across every symbol in files changed by a git diff. By default the diff is the working tree vs HEAD; supply ref_a and ref_b together to blast a ref range (e.g. main..HEAD) instead. Use for PR review or 'what does this branch touch?' - the seed is the diff, not a single node."

const DescSearchSemantic = "Natural-language search over embedded symbols (RRF-fused with FTS, lexical fallback when the embedder is offline). Best for behavior-shaped queries ('where do we validate session tokens'). Returns inline snippets so a follow-up Read is usually unnecessary. For known identifiers prefer eng_find_symbol (exact + deterministic); for 'what does this reach / who calls this' escalate to eng_get_call_chain / eng_get_blast_radius. With repo_id omitted (and cwd outside any registered repo) the query fanned out across every registered repo in parallel and is fused with a single GLOBAL RRF so a top hit in one repo competes fairly with a top hit in another; each result then carries 'repo_id' so callers can disambiguate. The returned score is intra-query RRF (~0.01–0.03 typical range); use rank, not absolute score, to compare hits."

const DescDepsImportOnlyCaveat = "a module imported but only referenced via struct literals / type assertions (no resolved package-qualified call) will not appear yet"

const DescFindSymbolMatching = "Unqualified names also match - 'Run' finds Server.Run, Command.Run, etc., with exact matches first."

const DescContextPack = "Bundle a symbol's neighborhood (callers, callees, adjacent tests, recent commits, open findings, active task) into one token-bounded payload. Use at the START of a non-trivial change so you don't have to assemble surrounding context piecewise. Surfaces cross_repo_edges in both directions, so cross-repo callers/callees show up in the same response. Token budget: the full pack runs up to ~8k tokens; if you only need 'what does X directly call', pass scope=focused for the seed + direct callees alone (much cheaper). Reserve the default scope=full for genuine understand-before-edit moments, not narrow lookups."
