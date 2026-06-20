"""Tests for eng_promote_repo - the post-commit hook target .

These exercise the contract a real post-commit-driven flow depends on:
the daemon, given a registered repo's root_path, re-Saves files changed
in HEAD and promotes them. Error paths must return loud RPC errors
rather than silently no-op'ing (the failure mode the legacy
{"cmd":"promote"} protocol had).
"""

from __future__ import annotations

from tests.mcp.helpers import query


def test_promote_repo_with_valid_root_advances_sha(mcp_client, repo_id):
    """Look up the repo's root, run eng_promote_repo, expect the daemon
    reports the promoted SHA. We assert structurally - the value is the
    HEAD git resolves to, which we don't know without re-shelling git."""
    root = query("SELECT root_path FROM repos WHERE repo_id = ?", (repo_id,))[0]["root_path"]
    ok, text, _, result = mcp_client.call("eng_promote_repo", {"root_path": root})
    assert ok, f"eng_promote_repo failed: {text}"
    assert result["repo_id"] == repo_id
    assert result["git_sha"], "git_sha should be non-empty"
    # FilesPromoted may be 0 when HEAD has no changed files relative to a
    # working tree the test couldn't perturb - that's fine; the contract
    # only requires the call to complete with a valid SHA.


def test_promote_repo_unknown_root_errors(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_promote_repo", {"root_path": "/nonexistent/path/zzz"})
    assert not ok
    assert "not registered" in text.lower(), (
        "want a clear 'not registered' error to differentiate from silent legacy behavior"
    )


def test_promote_repo_requires_root_path(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_promote_repo", {})
    assert not ok
    assert "required" in text.lower()
