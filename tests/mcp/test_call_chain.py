"""Tests for eng_get_call_chain — walks caller/callee edges from a node."""

from __future__ import annotations


def test_call_chain_for_target_node(mcp_client, repo_id, branch, target_symbol):
    _, _, _, find_result = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id, "branch": branch, "symbol": target_symbol,
    })
    nodes = find_result.get("nodes", [])
    assert nodes, "no nodes for target_symbol"
    node_id = nodes[0]["ID"]

    ok, text, _, result = mcp_client.call("eng_get_call_chain", {
        "repo_id": repo_id, "branch": branch,
        "node_id": node_id, "depth": 2,
    })
    assert ok, f"eng_get_call_chain failed: {text}"
    assert isinstance(result, dict)


def test_call_chain_requires_node_id(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_get_call_chain", {
        "repo_id": repo_id, "branch": branch, "depth": 2,
    })
    assert not ok and "required" in text.lower()
