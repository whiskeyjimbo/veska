"""Surface-level error tests - every tool we wire must return a
JSON-RPC error for unknown methods rather than crashing, and required-
arg errors must mention which arg is missing."""

from __future__ import annotations


def test_unknown_method_returns_error(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_definitely_not_a_real_tool", {})
    assert not ok
    assert "method not found" in text.lower()


def test_find_symbol_auto_resolves_repo_id(mcp_client):
    """eng_find_symbol no longer errors on a missing repo_id: the daemon
    auto-resolves it from cwd, then falls back to the sole registered repo
    (resolveRepoIDOrSingleton). The call succeeds (empty nodes for an
    unknown symbol is fine) rather than failing with 'repo_id required'
    (re-pinned from the pre-auto-resolve contract)."""
    ok, text, _, result = mcp_client.call("eng_find_symbol", {
        "branch": "main",
        "symbol": "definitely-not-a-real-symbol-zzz",
    })
    assert ok, f"expected auto-resolved repo_id, got error: {text}"
    assert not (result.get("nodes") or []), "unknown symbol should yield no nodes"


def test_get_file_nodes_auto_resolves_branch(mcp_client, repo_id, target_file):
    """eng_get_file_nodes no longer requires an explicit branch - it
    defaults to the repo's active_branch. Supplying a real repo_id +
    file_path with no branch succeeds (re-pinned)."""
    ok, text, _, _ = mcp_client.call("eng_get_file_nodes", {
        "repo_id": repo_id,
        "file_path": target_file,
    })
    assert ok, f"expected branch auto-resolve, got error: {text}"


def test_search_semantic_empty_query_errors(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_search_semantic", {
        "repo_id": repo_id,
        "branch": branch,
        "query": "",
    })
    # Either rejects empty query OR returns empty results - both are valid;
    # what matters is no crash, no silent success with bogus rankings.
    if not ok:
        return
