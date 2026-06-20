"""Tests for the repo-lifecycle MCP tools: eng_add_repo, eng_remove_repo,
eng_get_repo.

These mutate the registry. To leave the live state untouched we always
operate against a fresh temp git repo created per test, then remove it
when done."""

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


def test_add_and_remove_repo_roundtrip(mcp_client):
    with tempfile.TemporaryDirectory(prefix="veska-mcp-test-") as tmp:
        _init_repo(tmp)

        # Add.
        ok, text, _, result = mcp_client.call("eng_add_repo", {"root_path": tmp})
        assert ok, f"eng_add_repo failed: {text}"
        repo_id = result.get("repo_id")
        assert repo_id, "add_repo returned no repo_id"
        assert result.get("scan_pending") is True

        # Verify it appears via get_repo. The handler returns a nested
        # {"repo": {RepoID, RootPath, ActiveBranch, ...}} shape.
        ok2, text2, _, gr = mcp_client.call("eng_get_repo", {"repo_id": repo_id})
        assert ok2, f"eng_get_repo failed: {text2}"
        nested = gr.get("repo") if isinstance(gr, dict) else None
        got_id = (nested or gr).get("repo_id") if isinstance(nested or gr, dict) else None
        assert got_id == repo_id, f"eng_get_repo returned wrong id: {gr}"

        # Remove.
        ok3, text3, _, _ = mcp_client.call("eng_remove_repo", {"repo_id": repo_id})
        assert ok3, f"eng_remove_repo failed: {text3}"


def test_add_repo_idempotency_returns_already_registered(mcp_client):
    """A second eng_add_repo against the same root_path must
    report already_registered=true + scan_pending=false (no duplicate
    cold-scan dispatched) while still returning the original repo_id."""
    with tempfile.TemporaryDirectory(prefix="veska-mcp-test-") as tmp:
        _init_repo(tmp)
        try:
            ok, text, _, first = mcp_client.call("eng_add_repo", {"root_path": tmp})
            assert ok, f"first add failed: {text}"
            assert first.get("scan_pending") is True
            assert first.get("already_registered") in (False, None)
            repo_id = first["repo_id"]

            ok2, text2, _, second = mcp_client.call("eng_add_repo", {"root_path": tmp})
            assert ok2, f"second add failed: {text2}"
            assert second["repo_id"] == repo_id, "idempotent add returned a different id"
            assert second.get("already_registered") is True, (
                f"second add should report already_registered=True, got {second!r}"
            )
            assert second.get("scan_pending") is False, (
                f"second add should report scan_pending=False (no duplicate cold scan), got {second!r}"
            )
        finally:
            mcp_client.call("eng_remove_repo", {"repo_id": repo_id})


def test_add_repo_requires_root_path(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_add_repo", {})
    assert not ok and ("required" in text.lower() or "root_path" in text.lower())


def test_remove_repo_unknown_id_errors(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_remove_repo", {"repo_id": "not-a-real-repo-zzz"})
    # Either a domain error or a no-op success; what we forbid is method-
    # not-found.
    assert "method not found" not in text.lower()


def test_get_repo_unknown_id_responds(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_get_repo", {"repo_id": "not-a-real-repo-zzz"})
    # Tool must be registered; result/error shape doesn't matter here.
    assert "method not found" not in text.lower()
