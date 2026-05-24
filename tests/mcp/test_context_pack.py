"""Tests for eng_get_context_pack — bundles a node's neighbourhood for an
LLM prompt window."""

from __future__ import annotations


def test_context_pack_by_symbol(mcp_client, repo_id, branch, target_symbol):
    ok, text, _, result = mcp_client.call("eng_get_context_pack", {
        "repo_id": repo_id, "branch": branch, "symbol": target_symbol,
    })
    assert ok, f"eng_get_context_pack failed: {text}"
    assert isinstance(result, dict)
    # Real-content assertions: the pack carries the seed node, a token
    # budget, and the resolved repo_id + branch.
    assert result.get("repo_id") == repo_id
    assert result.get("branch") == branch
    assert result.get("mode") == "symbol"
    assert result.get("query") == target_symbol
    nodes = result.get("nodes") or []
    assert nodes, "expected at least the seed node"
    # The first node is the seed.
    assert nodes[0].get("seed") is True
    assert nodes[0].get("name") == target_symbol or target_symbol in nodes[0].get("path", "")
    assert result.get("token_budget", 0) > 0


def test_context_pack_branch_defaults_to_active(mcp_client, repo_id, target_symbol):
    """branch was required pre-solov2-5vu1; now it defaults to the
    registered active_branch when omitted. Omitting branch should
    succeed (not error)."""
    ok, text, _, result = mcp_client.call("eng_get_context_pack", {
        "repo_id": repo_id, "symbol": target_symbol,
    })
    assert ok, f"eng_get_context_pack without branch should default to active_branch, got error: {text}"
    assert isinstance(result, dict)
