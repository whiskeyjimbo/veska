"""Tests for eng_find_owner - surfaces the CODEOWNERS owner for a file."""

from __future__ import annotations


def test_find_owner_responds_for_target_file(mcp_client, repo_id, target_file):
    ok, text, _, result = mcp_client.call("eng_find_owner", {
        "repo_id": repo_id, "file_path": target_file,
    })
    # find_owner returns success even when no CODEOWNERS is configured -
    # the result just carries an empty owner list. Either outcome is fine
    # as long as the tool is registered and accepts the params.
    assert "method not found" not in text.lower()
    if ok:
        assert isinstance(result, dict)


def test_find_owner_requires_file_path(mcp_client, repo_id):
    ok, text, _, _ = mcp_client.call("eng_find_owner", {"repo_id": repo_id})
    assert not ok and "required" in text.lower()
