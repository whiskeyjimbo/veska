"""Tests for the findings MCP tools.

Findings represent durable lint/audit observations. The list/get path is
read-only and safe; close/reopen are mutating but we only exercise the
unknown-id path so we never alter the live state."""

from __future__ import annotations


def test_list_findings_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_list_findings", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_list_findings failed: {text}"
    # Findings list may be empty; just assert the response shape is sane.
    assert isinstance(result, dict)


def test_list_findings_state_filter(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_list_findings", {
        "repo_id": repo_id, "branch": branch, "state": "open",
    })
    assert ok, f"eng_list_findings with state=open failed: {text}"


def test_get_finding_missing_id_errors(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_get_finding", {
        "repo_id": repo_id, "branch": branch,
    })
    assert not ok and "required" in text.lower()


def test_close_finding_unknown_id_errors(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_close_finding", {
        "repo_id": repo_id, "branch": branch,
        "finding_id": "definitely-does-not-exist-xyz-9999",
        "reason": "harness probe",
    })
    # Either a clear domain error OR a successful no-op; what we must NOT
    # see is method-not-found (which would mean the tool was dropped).
    assert "method not found" not in text.lower()


def test_reopen_finding_unknown_id_errors(mcp_client, repo_id, branch):
    ok, text, _, _ = mcp_client.call("eng_reopen_finding", {
        "repo_id": repo_id, "branch": branch,
        "finding_id": "definitely-does-not-exist-xyz-9999",
    })
    assert "method not found" not in text.lower()
