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


def test_changed_symbols_defaults_to_last_commit(mcp_client, repo_id, branch):
    """Omitting both ref_a and ref_b must default to HEAD~1..HEAD rather
    than erroring (solov2-npjs). Previously this test asserted the
    opposite — the default landed in trunk and the test went stale."""
    ok, text, _, result = mcp_client.call("eng_find_changed_symbols", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_find_changed_symbols without refs should default to HEAD~1..HEAD, got error: {text}"
    assert isinstance(result, dict)
    # solov2-jbgt: empty buckets must serialize as [] (never null).
    for k in ("added", "removed", "modified"):
        assert k in result, f"missing key {k!r} in response: {result}"
        assert isinstance(result[k], list), f"{k!r} = {result[k]!r}, want list (solov2-jbgt)"


def test_changed_symbols_one_ref_alone_is_error(mcp_client, repo_id, branch):
    """Supplying only ref_a (or only ref_b) is ambiguous — must be rejected
    so the caller knows to pass both or neither (solov2-npjs)."""
    ok, text, _, _ = mcp_client.call("eng_find_changed_symbols", {
        "repo_id": repo_id, "branch": branch,
        "ref_a": "HEAD",
        # ref_b intentionally omitted
    })
    assert not ok and "together" in text.lower()


def test_changed_symbols_file_path_is_absolute(mcp_client, repo_id, branch):
    """solov2-w8nr: file_path in each SymbolChange must be absolute (it
    used to be repo-relative, inconsistent with every other node-emitting
    tool). HEAD~1..HEAD usually has at least one changed symbol; when
    the repo is one-commit-deep the test skips."""
    from tests.mcp.helpers import query
    root = query("SELECT root_path FROM repos WHERE repo_id = ?", (repo_id,))[0]["root_path"]
    ok, text, _, result = mcp_client.call("eng_find_changed_symbols", {
        "repo_id": repo_id, "branch": branch,
        "ref_a": "HEAD~1", "ref_b": "HEAD",
    })
    if not ok and "does not resolve" in text.lower():
        import pytest
        pytest.skip("repo has insufficient history (HEAD~1 unresolvable)")
    assert ok, f"eng_find_changed_symbols failed: {text}"
    seen_any = False
    for bucket in ("added", "removed", "modified"):
        for change in result.get(bucket) or []:
            seen_any = True
            fp = change.get("file_path", "")
            assert fp.startswith(root), (
                f"{bucket!r} entry file_path={fp!r} is not absolute under {root!r} (solov2-w8nr)"
            )
    if not seen_any:
        import pytest
        pytest.skip("no changes between HEAD~1 and HEAD on this fixture")
