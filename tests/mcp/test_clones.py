"""Tests for eng_find_clones - duplicate-code detection.

mode='exact' (the default) groups symbols whose source text is byte-identical;
the tool is read-only, so these run against the live promoted graph without
seeding. We assert the contract (responds, well-shaped, mode honored) rather
than the presence of clones, which is corpus-dependent."""

from __future__ import annotations


def test_find_clones_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_find_clones", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_find_clones failed: {text}"
    assert isinstance(result, dict)


def test_find_clones_groups_have_at_least_two_members(mcp_client, repo_id, branch):
    """A 'clone group' is meaningless with one member - every returned
    group must contain ≥2 symbols, the definition of a duplicate. Pinned
    to the real wire keys ('groups'[].'members') so a shape drift fails
    loudly here rather than passing on an empty-because-wrong-key list."""
    _, _, _, result = mcp_client.call("eng_find_clones", {
        "repo_id": repo_id, "branch": branch, "mode": "exact",
    })
    assert "groups" in result, f"missing 'groups' key - shape drift: {list(result)}"
    for g in result["groups"]:
        members = g.get("members")
        assert members is not None, f"clone group missing 'members': {g}"
        assert len(members) >= 2, f"clone group with <2 members: {g}"


def test_find_clones_requires_repo_id(mcp_client):
    """repo_id is mandatory even with a single repo registered - the tool
    does not auto-resolve it (it returns an explicit -32602 instead)."""
    ok, text, _, _ = mcp_client.call("eng_find_clones", {})
    assert not ok, "expected a required-param error for missing repo_id"
    assert "repo_id" in text.lower()
