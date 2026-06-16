"""Tests for eng_reindex_repo - forces a full cold-scan reparse.

Reindex is expensive and mutates last_promoted_sha + re-embeds, so we never
run it against the live registered repo (that would disrupt the shared
target_symbol / target_file fixtures). Every test operates on a fresh temp
git repo, removed in a finally - same hygiene as test_repo_lifecycle."""

from __future__ import annotations

import os
import subprocess
import tempfile


def _init_repo(tmp: str) -> None:
    for args in (
        ["init", "-q", "-b", "main"],
        ["config", "user.email", "harness@example.invalid"],
        ["config", "user.name", "harness"],
    ):
        subprocess.run(["git", "-C", tmp] + args, check=True)
    with open(os.path.join(tmp, "x.go"), "w") as f:
        f.write("package x\n\nfunc Probe() int { return 1 }\n")
    subprocess.run(["git", "-C", tmp, "add", "-A"], check=True)
    subprocess.run(["git", "-C", tmp, "commit", "-q", "-m", "init"], check=True)


def test_reindex_by_root_path(mcp_client):
    """Reindex a freshly added temp repo by root_path. Must resolve the
    repo and dispatch a scan, not method-not-found."""
    with tempfile.TemporaryDirectory(prefix="veska-mcp-reindex-") as tmp:
        _init_repo(tmp)
        ok, text, _, add = mcp_client.call("eng_add_repo", {"root_path": tmp})
        assert ok, f"eng_add_repo failed: {text}"
        repo_id = add["repo_id"]
        try:
            ok2, text2, _, result = mcp_client.call("eng_reindex_repo", {"root_path": tmp})
            assert ok2, f"eng_reindex_repo by root_path failed: {text2}"
            assert isinstance(result, dict)
        finally:
            mcp_client.call("eng_remove_repo", {"repo_id": repo_id})


def test_reindex_by_repo_id(mcp_client):
    """Reindex the same temp repo by repo_id."""
    with tempfile.TemporaryDirectory(prefix="veska-mcp-reindex-") as tmp:
        _init_repo(tmp)
        ok, text, _, add = mcp_client.call("eng_add_repo", {"root_path": tmp})
        assert ok, f"eng_add_repo failed: {text}"
        repo_id = add["repo_id"]
        try:
            ok2, text2, _, _ = mcp_client.call("eng_reindex_repo", {"repo_id": repo_id})
            assert ok2, f"eng_reindex_repo by repo_id failed: {text2}"
        finally:
            mcp_client.call("eng_remove_repo", {"repo_id": repo_id})


def test_reindex_unknown_repo_errors(mcp_client):
    """An unknown id must surface a domain error, never method-not-found."""
    ok, text, _, _ = mcp_client.call("eng_reindex_repo", {"repo_id": "not-a-real-repo-zzz"})
    assert "method not found" not in text.lower()
