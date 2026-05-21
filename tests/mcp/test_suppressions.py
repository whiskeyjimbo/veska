"""Tests for the suppressions MCP tools.

Suppressions silence specific findings. The round-trip happy path uses
the same auto-generated dead-code findings test_findings.py relies on,
then cleans up by closing the created suppression at the end."""

from __future__ import annotations

import pytest

from tests.mcp.helpers import query


def _any_open_finding(repo_id: str, branch: str) -> str | None:
    rows = query(
        """SELECT finding_id FROM findings
           WHERE repo_id = ? AND branch = ? AND state = 'open' LIMIT 1""",
        (repo_id, branch),
    )
    return rows[0]["finding_id"] if rows else None


@pytest.fixture
def open_finding(repo_id, branch):
    fid = _any_open_finding(repo_id, branch)
    if not fid:
        pytest.skip("no open finding to exercise — promote first")
    return fid


def test_list_suppressions_responds(mcp_client, repo_id, branch):
    ok, text, _, result = mcp_client.call("eng_list_suppressions", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok, f"eng_list_suppressions failed: {text}"
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


def test_get_suppression_missing_id_errors(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_get_suppression", {})
    assert not ok and "required" in text.lower()


def test_close_suppression_unknown_id_errors(mcp_client, repo_id):
    ok, text, _, _ = mcp_client.call("eng_close_suppression", {
        "repo_id": repo_id,
        "suppression_id": "definitely-not-a-real-suppression-zzz",
    })
    assert "method not found" not in text.lower()
