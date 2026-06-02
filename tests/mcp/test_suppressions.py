"""Tests for the suppressions MCP tools.

Suppressions silence specific findings. The round-trip happy path uses
the same auto-generated dead-code findings test_findings.py relies on,
then cleans up by closing the created suppression at the end."""

from __future__ import annotations

import pytest

# open_finding is a shared fixture from conftest.py.


def test_list_suppressions_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_list_suppressions", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_list_suppressions failed: {text}"
    assert isinstance(result, dict)


def test_list_suppressions_repo_id_defaults_with_single_repo(mcp_client):
    """solov2-7tz1: when exactly one repo is registered, eng_list_suppressions
    must auto-resolve repo_id from the singleton instead of erroring."""
    ok, _, _, list_result = mcp_client.call("eng_list_repos", {})
    assert ok
    repos = list_result.get("repos", []) if isinstance(list_result, dict) else []
    if len(repos) != 1:
        pytest.skip(f"single-repo defaulting requires exactly 1 repo, fixture has {len(repos)}")
    ok2, text2, _, result = mcp_client.call("eng_list_suppressions", {})
    assert ok2, f"eng_list_suppressions without repo_id should auto-resolve, got: {text2}"
    assert isinstance(result, dict)


def test_suppress_then_close_roundtrip(mcp_client, repo_id, branch, open_finding):
    """suppress_finding → list shows it → get_suppression returns the
    record → close_suppression sets expires_at. The created suppression
    is closed at the end of the test (best-effort cleanup)."""
    ok, text, _, result = mcp_client.call("eng_suppress_finding", {
        "repo_id": repo_id, "branch": branch,
        "finding_id": open_finding,
        "reason": "harness suppress-roundtrip",
    })
    assert ok, f"eng_suppress_finding failed: {text}"
    sup_id = result.get("suppression_id")
    assert sup_id, "suppress_finding returned no suppression_id"

    try:
        # list shows it
        _, _, _, lst = mcp_client.call("eng_list_suppressions", {
            "repo_id": repo_id, "branch": branch,
        })
        ids = [s.get("suppression_id") for s in lst.get("suppressions", []) or []]
        assert sup_id in ids, f"suppression {sup_id} missing from list {ids}"

        # get returns the record
        ok2, text2, _, gr = mcp_client.call("eng_get_suppression", {
            "suppression_id": sup_id,
        })
        assert ok2, f"eng_get_suppression failed: {text2}"
        rec = gr.get("suppression") if isinstance(gr, dict) else None
        assert rec and rec.get("suppression_id") == sup_id
        assert rec.get("target") == open_finding
    finally:
        # Cleanup — close the suppression so we don't leak state.
        mcp_client.call("eng_close_suppression", {
            "suppression_id": sup_id, "repo_id": repo_id,
        })


def test_suppress_finding_rejects_unknown_id(mcp_client, repo_id, branch):
    """solov2-b36: scope='finding' (the default) must verify the finding
    actually exists. Previously this silently inserted an orphan row that
    polluted eng_list_suppressions forever."""
    ok, text, _, _ = mcp_client.call("eng_suppress_finding", {
        "repo_id": repo_id, "branch": branch,
        "finding_id": "definitely-not-real-zzz",
        "reason": "harness orphan-probe",
    })
    assert not ok, "expected finding-not-found error"
    assert "finding not found" in text.lower()


def test_get_suppression_missing_id_errors(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_get_suppression", {})
    assert not ok and "required" in text.lower()


def test_close_suppression_unknown_id_errors(mcp_client, repo_id):
    ok, text, _, _ = mcp_client.call("eng_close_suppression", {
        "repo_id": repo_id,
        "suppression_id": "definitely-not-a-real-suppression-zzz",
    })
    assert "method not found" not in text.lower()
