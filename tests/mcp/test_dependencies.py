"""Tests for eng_list_dependencies — external modules the repo calls into,
ranked by call-site count. Read-only; runs against the live graph."""

from __future__ import annotations


def test_list_dependencies_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_list_dependencies", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_list_dependencies failed: {text}"
    assert isinstance(result, dict)


def test_list_dependencies_ranked_descending(mcp_client, repo_id, branch):
    """The tool documents 'ranked by call-site count' — verify usage_count
    is monotonically non-increasing so the ranking contract holds. Pinned
    to the real wire keys ('dependencies'[].'usage_count', per
    application/dependencies/dependencies.go) so a missing key fails loudly
    instead of evaporating into an empty list that trivially 'sorts'."""
    _, _, _, result = mcp_client.call("eng_list_dependencies", {
        "repo_id": repo_id, "branch": branch,
    })
    assert "dependencies" in result, f"missing 'dependencies' key — shape drift: {list(result)}"
    deps = result["dependencies"]
    counts = [d["usage_count"] for d in deps]
    assert counts == sorted(counts, reverse=True), (
        f"dependencies not ranked by usage_count descending: {counts}"
    )


def test_list_dependencies_requires_repo_id(mcp_client):
    """repo_id is mandatory even with a single repo registered — the tool
    returns an explicit -32602 rather than auto-resolving."""
    ok, text, _, _ = mcp_client.call("eng_list_dependencies", {})
    assert not ok, "expected a required-param error for missing repo_id"
    assert "repo_id" in text.lower()
