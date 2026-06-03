"""Tests for eng_get_call_chain — walks caller/callee edges from a node."""

from __future__ import annotations


def test_call_chain_for_target_node(mcp_client, repo_id, branch, target_symbol):
    _, _, _, find_result = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id, "branch": branch, "symbol": target_symbol,
    })
    nodes = find_result.get("nodes", [])
    assert nodes, "no nodes for target_symbol"
    node_id = nodes[0]["node_id"]

    ok, text, _, result = mcp_client.call("eng_get_call_chain", {
        "repo_id": repo_id, "branch": branch,
        "node_id": node_id, "depth": 2,
    })
    assert ok, f"eng_get_call_chain failed: {text}"
    assert isinstance(result, dict)
    # included_staging is always set — pin it as a structure smoke.
    assert "included_staging" in result


def test_call_chain_unknown_node_errors(mcp_client, repo_id, branch):
    """eng_get_call_chain now LOUDLY rejects an unknown node_id rather than
    soft-failing with an empty body: the shared node-id resolver (solov2-izh6,
    "junior-engineer journey gaps") returns -32002 with a "node_id … not in
    repo … may belong to a different registered repo" hint across every
    node-id tool. That's more useful to an agent than a silent empty result,
    so it's the canonical contract (solov2-khra: re-pinned from soft-fail)."""
    ok, text, _, _ = mcp_client.call("eng_get_call_chain", {
        "repo_id": repo_id, "branch": branch,
        "node_id": "definitely-not-a-real-node-deadbeef",
        "depth": 2,
    })
    assert not ok, "unknown node_id should surface a loud resolver error"
    assert "not in repo" in text.lower()


def test_call_chain_requires_node_id_or_symbol(mcp_client, repo_id, branch):
    """solov2-lcz6: call_chain now accepts node_id OR symbol; supplying
    neither must still error with a 'required' message."""
    ok, text, _, _ = mcp_client.call("eng_get_call_chain", {
        "repo_id": repo_id, "branch": branch, "depth": 2,
    })
    assert not ok and "required" in text.lower()


def test_call_chain_accepts_symbol(mcp_client, repo_id, branch, target_symbol):
    """solov2-lcz6: passing 'symbol' instead of 'node_id' must resolve via
    FindNodes and return the same shape as the node_id path. Ambiguous
    symbols are rejected (separate case)."""
    ok, text, _, result = mcp_client.call("eng_get_call_chain", {
        "repo_id": repo_id, "branch": branch,
        "symbol": target_symbol, "depth": 2,
    })
    assert ok, f"call_chain with symbol={target_symbol!r} should succeed, got: {text}"
    assert isinstance(result, dict)
    assert "included_staging" in result
