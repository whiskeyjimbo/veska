"""Tests for the admin MCP tools (status, repo listing, config)."""

from __future__ import annotations


def test_get_status_responds(mcp_client, fixture_summary):
    ok, text, _, result = mcp_client.call("eng_get_status", {})
    assert ok, f"eng_get_status failed: {text}"
    assert result.get("status") == "ok"
    assert result.get("schema_version", 0) >= 9
    # scans_in_flight (solov2-pm5) is always present. Idle daemons return
    # an empty list; while a cold scan is running it lists the repo_id.
    assert "scans_in_flight" in result, "expected scans_in_flight key in get_status"
    sif = result["scans_in_flight"]
    assert isinstance(sif, list)
    for s in sif:
        assert "repo_id" in s and "phase" in s and "started_at" in s


def test_list_repos_includes_target(mcp_client, repo_id):
    ok, text, _, result = mcp_client.call("eng_list_repos", {})
    assert ok, f"eng_list_repos failed: {text}"
    ids = [r["repo_id"] for r in result.get("repos", [])]
    assert repo_id in ids, f"target repo {repo_id} not in {ids}"


def test_get_current_repo_requires_cwd(mcp_client):
    """eng_get_current_repo errors when cwd is omitted."""
    ok, text, _, _ = mcp_client.call("eng_get_current_repo", {})
    assert not ok and "cwd" in text.lower()


def test_get_current_repo_resolves_known_root(mcp_client, repo_id):
    """When cwd is set to a registered repo's RootPath, the returned
    repo's RepoID matches."""
    from tests.mcp.helpers import query
    root = query("SELECT root_path FROM repos WHERE repo_id = ?", (repo_id,))[0]["root_path"]
    ok, text, _, result = mcp_client.call("eng_get_current_repo", {"cwd": root})
    assert ok, f"eng_get_current_repo failed: {text}"
    rec = result.get("repo") if isinstance(result, dict) else None
    assert rec and rec.get("repo_id") == repo_id


def test_get_repo_unknown_id_errors(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_get_repo", {"repo_id": "definitely-not-a-real-repo"})
    assert not ok and "not found" in text.lower()


def test_get_config_responds(mcp_client):
    ok, text, _, result = mcp_client.call("eng_get_config", {})
    assert ok, f"eng_get_config failed: {text}"
    # vector_backend is one of the few stable keys we can pin.
    assert "vector_backend" in result
