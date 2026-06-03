"""Tests for the findings MCP tools.

Findings are durable lint/audit observations the daemon emits during
promotion (e.g. dead-code rules). The journey-style fixture repo
auto-generates a couple of low-severity dead-code findings, so we can
exercise the full lifecycle here without seeding any data."""

from __future__ import annotations

# open_finding is a shared fixture from conftest.py.


def test_list_findings_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_list_findings", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_list_findings failed: {text}"
    assert isinstance(result, dict)


def test_list_findings_state_filter(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_list_findings", {
        "repo_id": repo_id, "branch": branch, "state": "open",
    })
    assert ok, f"eng_list_findings state=open failed: {text}"
    for f in result.get("findings", []) or []:
        assert f.get("state") == "open"


def test_get_finding_happy(mcp_client, repo_id, branch, open_finding):
    ok, text, _, result = mcp_client.call("eng_get_finding", {
        "repo_id": repo_id, "branch": branch, "finding_id": open_finding,
    })
    assert ok, f"eng_get_finding failed: {text}"
    rec = result.get("finding") if isinstance(result, dict) else None
    assert rec, "expected a 'finding' object in result"
    assert rec.get("finding_id") == open_finding


def test_get_finding_missing_id_errors(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_get_finding", {
        "repo_id": repo_id, "branch": branch,
    })
    assert not ok and "required" in text.lower()


def test_close_then_reopen_finding_roundtrip(mcp_client, repo_id, branch, open_finding):
    """Close → reopen the same finding and assert the state transitions
    are visible via eng_get_finding. Leaves the finding in its original
    'open' state."""
    ok1, text1, _, r1 = mcp_client.call("eng_close_finding", {
        "repo_id": repo_id, "branch": branch,
        "finding_id": open_finding, "reason": "harness roundtrip",
    })
    assert ok1, f"close failed: {text1}"
    assert r1.get("state") == "closed"

    ok2, text2, _, r2 = mcp_client.call("eng_reopen_finding", {
        "repo_id": repo_id, "branch": branch, "finding_id": open_finding,
    })
    assert ok2, f"reopen failed: {text2}"
    assert r2.get("state") == "open"

    # Confirm via eng_get_finding that the underlying row reflects 'open'.
    _, _, _, gr = mcp_client.call("eng_get_finding", {
        "repo_id": repo_id, "branch": branch, "finding_id": open_finding,
    })
    rec = gr.get("finding") if isinstance(gr, dict) else None
    assert rec and rec.get("state") == "open"


def test_close_finding_unknown_id_errors(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_close_finding", {
        "repo_id": repo_id, "branch": branch,
        "finding_id": "definitely-does-not-exist-xyz-9999",
        "reason": "harness probe",
    })
    assert "method not found" not in text.lower()
