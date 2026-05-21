"""Tests for eng_get_context_pack — bundles a node's neighbourhood for an
LLM prompt window."""

from __future__ import annotations


def test_context_pack_by_symbol(mcp_client, repo_id, branch, target_symbol):
    ok, text, _, result = mcp_client.call("eng_get_context_pack", {
        "repo_id": repo_id, "branch": branch, "symbol": target_symbol,
    })
    assert ok, f"eng_get_context_pack failed: {text}"
    assert isinstance(result, dict)


def test_context_pack_requires_branch(mcp_client, repo_id, target_symbol):
    ok, text, _, _ = mcp_client.call("eng_get_context_pack", {
        "repo_id": repo_id, "symbol": target_symbol,
    })
    assert not ok and "required" in text.lower()
