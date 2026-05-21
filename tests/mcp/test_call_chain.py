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
    # included_staging is always set — pin it as a structure smoke.
    assert "included_staging" in result


def test_call_chain_unknown_node_soft_fails(mcp_client, repo_id, branch):
    """The journey test confirmed eng_get_call_chain returns success with
    an empty body when the node_id doesn't exist — not an error. Pin
    that contract so a future change that 'helpfully' errors on missing
    nodes shows up here."""
    ok, _, _, result = mcp_client.call("eng_get_call_chain", {
        "repo_id": repo_id, "branch": branch,
        "node_id": "definitely-not-a-real-node-deadbeef",
        "depth": 2,
    })
    assert ok, "unknown node should soft-fail with empty body, not error"
    # No nodes/edges keys means an empty body — acceptable here.
    assert not result.get("nodes") and not result.get("edges")


def test_call_chain_requires_node_id(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_get_call_chain", {
        "repo_id": repo_id, "branch": branch, "depth": 2,
    })
    assert not ok and "required" in text.lower()
