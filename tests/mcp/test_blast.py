"""Tests for the blast-radius MCP tools (node-scoped, diff-scoped, dirty)."""

from __future__ import annotations

import pytest


def test_blast_radius_for_target_node(mcp_client, repo_id, branch, target_symbol):
    """eng_get_blast_radius requires a node_id. Resolve target_symbol →
    node_id via eng_find_symbol, then walk its blast radius."""
    _, _, _, find_result = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id, "branch": branch, "symbol": target_symbol,
    })
    nodes = find_result.get("nodes", [])
    assert nodes, "cannot test blast radius without a target node"
    node_id = nodes[0]["ID"]

    ok, text, _, result = mcp_client.call("eng_get_blast_radius", {
        "repo_id": repo_id, "branch": branch,
        "node_id": node_id, "max_depth": 2, "max_nodes": 50,
    })
    assert ok, f"eng_get_blast_radius failed: {text}"
    assert isinstance(result, dict)


def test_blast_radius_requires_node_id(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_get_blast_radius", {
        "repo_id": repo_id, "branch": branch,
    })
    assert not ok and "required" in text.lower()


def test_dirty_blast_radius_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_get_dirty_blast_radius", {
        "repo_id": repo_id, "branch": branch,
    })
    # Either succeeds (with truncated:false or a list of entries) or errors
    # cleanly when the repo has no dirty files; what we forbid is a method-
    # not-found or a crash.
    assert "method not found" not in text.lower()
    if ok:
        assert isinstance(result, dict)


def test_diff_blast_radius_responds(mcp_client, repo_id, branch):
    # diff_blast_radius takes (repo_id, branch, ref_a, ref_b) in its full
    # form, but the read accepts defaulted refs (HEAD~1..HEAD). Probing
    # with no extra args is enough to confirm the tool is registered and
    # responds.
    _, text, _, _ = mcp_client.call("eng_get_diff_blast_radius", {
        "repo_id": repo_id, "branch": branch,
    })
    assert "method not found" not in text.lower()
