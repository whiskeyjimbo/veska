"""Cross-cutting tests: every registered MCP tool is discoverable. Acts as
a smoke test for the whole surface — if a future change drops a tool we
catch it here rather than in whichever per-tool file happens to exercise
it. The list mirrors `grep -rn 'Name:\\s*\"eng_' internal/infrastructure/mcp`
and matches cmd/veska-daemon/wire.go's registerMCPTools."""

from __future__ import annotations

ALL_TOOLS = {
    # admin
    "eng_get_config",
    "eng_get_current_repo",
    "eng_get_repo",
    "eng_get_status",
    "eng_list_repos",
    # repo lifecycle
    "eng_add_repo",
    "eng_remove_repo",
    "eng_promote_repo",  # solov2-3vv post-commit hook target
    # graph
    "eng_find_symbol",
    "eng_get_node",
    "eng_get_file_nodes",
    "eng_get_call_chain",
    # blast radius
    "eng_get_blast_radius",
    "eng_get_diff_blast_radius",
    "eng_get_dirty_blast_radius",
    # search
    "eng_search_semantic",
    "eng_search_similar",
    # context pack
    "eng_get_context_pack",
    # changed symbols
    "eng_find_changed_symbols",
    # todos
    "eng_find_todos",
    # owner
    "eng_find_owner",
    # tasks (PARKED — solov2-6m1; no MCP path to create a task, so the
    # set/get/history tools are unregistered until a backend lands).
    # findings
    "eng_list_findings",
    "eng_get_finding",
    "eng_close_finding",
    "eng_reopen_finding",
    # suppressions
    "eng_list_suppressions",
    "eng_get_suppression",
    "eng_suppress_finding",
    "eng_close_suppression",
    # wiki
    "eng_get_hot_zone",
    "eng_get_entry_points",
    # clones / similarity
    "eng_find_clones",  # solov2-wfrj
    "eng_find_related",
    # dependencies
    "eng_list_dependencies",
    # reindex
    "eng_reindex_repo",
    # aliases
    "eng_set_repo_alias",
    "eng_remove_repo_alias",
}


def test_known_tools_all_registered(mcp_client):
    """Every registered tool must respond — either with a result or with
    a domain error — when called with an empty payload. method-not-found
    means the tool was dropped from wire.go's registerMCPTools."""
    missing = []
    for tool in sorted(ALL_TOOLS):
        _, text, _, _ = mcp_client.call(tool, {})
        if "method not found" in text.lower():
            missing.append(tool)
    assert not missing, f"tools missing from registry: {missing}"


def test_tool_count_matches_expectation(mcp_client):
    """If a new tool lands in wire.go but not in ALL_TOOLS, this test fails
    loudly so a contributor remembers to add coverage for it. The count
    matches wire_test.go's TestWire_RegistersFinalFiveTools assertion (37)."""
    assert len(ALL_TOOLS) == 37, (
        f"ALL_TOOLS has {len(ALL_TOOLS)} entries; wire.go registers 37. "
        "Update tests/mcp/test_meta.ALL_TOOLS and add a per-tool test file."
    )


def test_all_tools_matches_live_catalog(mcp_client):
    """ALL_TOOLS must equal the daemon's live tools/list catalog. This is
    the self-maintaining guard: a tool added to wire.go but not to
    ALL_TOOLS (or vice-versa) fails here with the exact diff, so the
    hardcoded set above can't silently drift from what the server serves —
    the failure mode that left ALL_TOOLS stale at 31 while wire.go shipped
    37 (solov2-seut cleanup)."""
    _, text, _, result = mcp_client.call("tools/list", {})
    live = {t["name"] for t in (result.get("tools") or [])}
    assert live, f"tools/list returned no tools: {text}"
    missing_from_tests = live - ALL_TOOLS
    stale_in_tests = ALL_TOOLS - live
    assert not (missing_from_tests or stale_in_tests), (
        f"ALL_TOOLS drifted from live catalog — "
        f"registered but untracked: {sorted(missing_from_tests)}; "
        f"tracked but not registered: {sorted(stale_in_tests)}"
    )
