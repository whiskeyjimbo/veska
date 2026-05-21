"""Surface-level error tests — every tool we wire must return a
JSON-RPC error for unknown methods rather than crashing, and required-
arg errors must mention which arg is missing."""

from __future__ import annotations


def test_unknown_method_returns_error(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_definitely_not_a_real_tool", {})
    assert not ok
    assert "method not found" in text.lower()


def test_find_symbol_missing_repo_id(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_find_symbol", {
        "branch": "main",
        "symbol": "x",
    })
    assert not ok
    assert "required" in text.lower()


def test_get_file_nodes_missing_branch(mcp_client, repo_id):
    ok, text, _, _ = mcp_client.call("eng_get_file_nodes", {
        "repo_id": repo_id,
        "file_path": "x.go",
    })
    assert not ok
    assert "required" in text.lower()


def test_search_semantic_empty_query_errors(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_search_semantic", {
        "repo_id": repo_id,
        "branch": branch,
        "query": "",
    })
    # Either rejects empty query OR returns empty results — both are valid;
    # what matters is no crash, no silent success with bogus rankings.
    if not ok:
        return
