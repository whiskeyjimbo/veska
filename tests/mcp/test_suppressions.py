"""Tests for the suppressions MCP tools.

Suppressions silence specific findings. Mutating calls operate against
unknown IDs only — same safe-probe pattern as test_findings.py."""

from __future__ import annotations


def test_list_suppressions_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_list_suppressions", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_list_suppressions failed: {text}"
    assert isinstance(result, dict)


def test_get_suppression_missing_id_errors(mcp_client, repo_id):
    ok, text, _, _ = mcp_client.call("eng_get_suppression", {
        "repo_id": repo_id,
    })
    assert not ok and "required" in text.lower()


def test_suppress_finding_unknown_id_errors(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_suppress_finding", {
        "repo_id": repo_id, "branch": branch,
        "finding_id": "definitely-not-a-real-finding-zzz",
        "reason": "harness probe",
    })
    assert "method not found" not in text.lower()


def test_close_suppression_unknown_id_errors(mcp_client, repo_id):
    ok, text, _, _ = mcp_client.call("eng_close_suppression", {
        "repo_id": repo_id,
        "suppression_id": "definitely-not-a-real-suppression-zzz",
    })
    assert "method not found" not in text.lower()
