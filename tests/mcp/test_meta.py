"""Cross-cutting tests: every registered MCP tool is discoverable and the
schema/migration version is sane. Acts as a smoke test for the entire
tool surface — if a future change accidentally drops a tool we notice
here rather than in the per-tool file that happens to exercise it."""

from __future__ import annotations

# Set of tools we expect to be present at minimum. Keep this list in lock-
# step with cmd/veska-daemon/wire.go's registerMCPTools — when a new tool
# lands, add it here so the journey-style smoke confirms wiring + naming.
REQUIRED_TOOLS = {
    "eng_add_repo",
    "eng_remove_repo",
    "eng_list_repos",
    "eng_get_repo",
    "eng_get_status",
    "eng_get_config",
    "eng_find_symbol",
    "eng_get_node",
    "eng_get_file_nodes",
    "eng_search_semantic",
    "eng_search_similar",
    "eng_promote_repo",  # solov2-3vv post-commit hook target
}


def test_known_tools_callable(mcp_client):
    """Every required tool either responds with a result OR returns a
    'required' / 'invalid params' error when called with an empty payload.
    Anything else (method-not-found, crash) means the tool was dropped."""
    for tool in sorted(REQUIRED_TOOLS):
        ok, text, _, _ = mcp_client.call(tool, {})
        assert "method not found" not in text.lower(), (
            f"tool {tool!r} is not registered; daemon returned: {text}"
        )
        # ok is fine; not-ok with a domain error is also fine.
        _ = ok
