"""Tests for eng_find_todos — surfaces parser-emitted TODO/FIXME comments."""

from __future__ import annotations


def test_find_todos_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_find_todos", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_find_todos failed: {text}"
    assert isinstance(result, dict)
    # Each todo (when present) carries Author, Body, FilePath at minimum.
    for t in result.get("todos", []) or []:
        assert "Body" in t or "body" in t


def test_find_todos_include_closed(mcp_client, repo_id, branch):
    ok, _, _, _ = mcp_client.call("eng_find_todos", {
        "repo_id": repo_id, "branch": branch, "include_closed": True,
    })
    assert ok


def test_find_todos_requires_branch(mcp_client, repo_id):
    ok, text, _, _ = mcp_client.call("eng_find_todos", {"repo_id": repo_id})
    assert not ok and "required" in text.lower()
