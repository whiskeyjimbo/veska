"""Tests for eng_find_todos - surfaces parser-emitted TODO/FIXME comments."""

from __future__ import annotations


def test_find_todos_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_find_todos", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_find_todos failed: {text}"
    assert isinstance(result, dict)
    # Wire-shape keys are snake_case. Each todo carries finding_id,
    # file_path, message, state, etc.
    for t in result.get("todos", []) or []:
        assert "file_path" in t and "message" in t, f"unexpected todo shape: {t}"


def test_find_todos_include_closed(mcp_client, repo_id, branch):
    ok, _, _, _ = mcp_client.call("eng_find_todos", {
        "repo_id": repo_id, "branch": branch, "include_closed": True,
    })
    assert ok


def test_find_todos_branch_defaults_to_active(mcp_client, repo_id):
    """branch was required pre-solov2-5vu1; now optional. Omitting branch
    should succeed (resolves to the repo's active_branch)."""
    ok, text, _, result = mcp_client.call("eng_find_todos", {"repo_id": repo_id})
    assert ok, f"eng_find_todos without branch should default to active_branch, got: {text}"
    assert isinstance(result, dict)
