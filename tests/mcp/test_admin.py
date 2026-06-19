"""Tests for the admin MCP tools (status, repo listing, config)."""

from __future__ import annotations

import os

from tests.mcp.helpers import assert_healthy_status, query


def test_get_status_responds(mcp_client, fixture_summary):
    ok, text, _, result = mcp_client.call("eng_get_status", {})
    assert ok, f"eng_get_status failed: {text}"
    assert_healthy_status(result)
    assert result.get("schema_version", 0) >= 9
    # scans_in_flight  is always present. Idle daemons return
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


def test_get_current_repo_uses_injected_cwd(mcp_client):
    """The veska-mcp shim injects os.Getwd() as `cwd` into every eng_* call
    that omits it (cwd_inject.go), and for eng_get_current_repo cwd is the
    sole resolution signal. So through the shim, calling with no cwd does
    NOT hit the daemon's sole-repo fallback (that path is reachable only by
    a direct daemon client and is covered by the daemon unit tests) - it
    resolves against the test runner's cwd. Since the runner sits in the
    veska source tree, not a registered repo, the daemon answers with the
    loud 'no indexed repo found for cwd' error.

    (the prior assertion expected sole-repo auto-resolve here,
    which the cwd-injecting shim makes unreachable.)"""
    rid = query(
        "SELECT repo_id FROM repos WHERE ? LIKE root_path || '%'",
        (os.getcwd(),),
    )
    if rid:
        import pytest
        pytest.skip("test runner cwd is itself a registered repo")

    ok, text, _, _ = mcp_client.call("eng_get_current_repo", {})
    assert not ok, f"expected unresolved-cwd error, got success: {text}"
    assert "no indexed repo found for cwd" in text.lower()


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
