"""Tests for eng_find_changed_symbols — diffs symbol sets between two refs."""

from __future__ import annotations


def test_changed_symbols_head_against_head(mcp_client, repo_id, branch):
    # HEAD against itself is a degenerate diff that should succeed with
    # an empty result. Useful as a smoke test that doesn't require knowing
    # any specific historic ref.
    ok, text, _, result = mcp_client.call("eng_find_changed_symbols", {
        "repo_id": repo_id, "branch": branch,
        "ref_a": "HEAD", "ref_b": "HEAD",
    })
    assert ok, f"eng_find_changed_symbols failed: {text}"
    assert isinstance(result, dict)


def test_changed_symbols_requires_refs(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_find_changed_symbols", {
        "repo_id": repo_id, "branch": branch,
    })
    assert not ok and "required" in text.lower()
