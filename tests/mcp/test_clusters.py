"""Tests for eng_find_clusters — unified, tier-labeled similar-code clusters.

One pass returns exact / structural / near groups for de-dupe triage. Read-only,
so these assert the contract (responds, well-shaped, tiers/scope honoured) rather
than the presence of clones, which is corpus-dependent."""

from __future__ import annotations


def test_find_clusters_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_find_clusters", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_find_clusters failed: {text}"
    assert isinstance(result, dict)
    assert "clusters" in result, f"missing 'clusters' key — shape drift: {list(result)}"


def test_find_clusters_groups_are_well_formed(mcp_client, repo_id, branch):
    """Every cluster has >=2 members and a valid tier; members carry repo_id."""
    _, _, _, result = mcp_client.call("eng_find_clusters", {
        "repo_id": repo_id, "branch": branch,
    })
    for c in result.get("clusters", []):
        assert c.get("tier") in ("exact", "structural", "near"), f"bad tier: {c}"
        members = c.get("members")
        assert members and len(members) >= 2, f"cluster with <2 members: {c}"
        for m in members:
            assert "repo_id" in m and "file_path" in m, f"member missing fields: {m}"


def test_find_clusters_tier_filter_honoured(mcp_client, repo_id, branch):
    """A tiers=exact filter must never return a structural/near cluster."""
    _, _, _, result = mcp_client.call("eng_find_clusters", {
        "repo_id": repo_id, "branch": branch, "tiers": "exact",
    })
    for c in result.get("clusters", []):
        assert c["tier"] == "exact", f"tiers=exact leaked a {c['tier']} cluster"


def test_find_clusters_rejects_bad_scope(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_find_clusters", {
        "repo_id": repo_id, "branch": branch, "scope": "galaxy",
    })
    assert not ok, "expected an invalid-param error for an unknown scope"
    assert "scope" in text.lower()
