"""Tests for the wiki MCP tools (hot zones + entry points).

Wiki surfaces are derived state — they may be empty for a freshly
indexed repo. We check the tools are registered, accept the standard
{repo_id, branch} params, and return well-shaped (possibly empty)
responses."""

from __future__ import annotations


def test_get_hot_zone_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_get_hot_zone", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_get_hot_zone failed: {text}"
    assert isinstance(result, dict)


def test_get_entry_points_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_get_entry_points", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_get_entry_points failed: {text}"
    assert isinstance(result, dict)


def test_get_hot_zone_branch_defaults_to_active(mcp_client, repo_id):
    """branch was required pre-solov2-5vu1; now optional. Omitting branch
    should succeed (resolves to the repo's active_branch)."""
    ok, text, _, result = mcp_client.call("eng_get_hot_zone", {"repo_id": repo_id})
    assert ok, f"eng_get_hot_zone without branch should default to active_branch, got: {text}"
    assert isinstance(result, dict)


def test_get_entry_points_branch_defaults_to_active(mcp_client, repo_id):
    """branch was required pre-solov2-5vu1; now optional."""
    ok, text, _, result = mcp_client.call("eng_get_entry_points", {"repo_id": repo_id})
    assert ok, f"eng_get_entry_points without branch should default to active_branch, got: {text}"
    assert isinstance(result, dict)
