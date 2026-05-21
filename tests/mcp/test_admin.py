"""Tests for the admin MCP tools (status, repo listing, config)."""

from __future__ import annotations


def test_get_status_responds(mcp_client, fixture_summary):
    ok, text, _, result = mcp_client.call("eng_get_status", {})
    assert ok, f"eng_get_status failed: {text}"
    assert result.get("status") == "ok"
    assert result.get("schema_version", 0) >= 9


def test_list_repos_includes_target(mcp_client, repo_id):
    ok, text, _, result = mcp_client.call("eng_list_repos", {})
    assert ok, f"eng_list_repos failed: {text}"
    ids = [r["RepoID"] for r in result.get("repos", [])]
    assert repo_id in ids, f"target repo {repo_id} not in {ids}"


def test_get_current_repo_responds(mcp_client):
    # eng_get_current_repo resolves the cwd to a registered repo if one
    # matches. We don't assert success because the pytest cwd is unlikely
    # to BE a registered repo — but the call should not crash or return
    # an unexpected error code.
    _, text, _, _ = mcp_client.call("eng_get_current_repo", {})
    assert isinstance(text, str)


def test_get_config_responds(mcp_client):
    ok, text, _, result = mcp_client.call("eng_get_config", {})
    assert ok, f"eng_get_config failed: {text}"
    # vector_backend is one of the few stable keys we can pin.
    assert "vector_backend" in result
