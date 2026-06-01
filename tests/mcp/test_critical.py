"""Critical paths — invariants that must hold across the system.

These tests pin contracts that, if broken, mean silent data loss or
silent incorrectness. They are run-against-the-live-daemon checks; the
pytest expressions are cross-validation against SQLite ground truth.

Most assertions here pin bugs we've already fixed in this codebase:
  - solov2-c47: repos.last_promoted_sha advances atomically with nodes
  - solov2-sxa: nodes.snippet is populated by the promotion path
  - solov2-249: sqlite-vec rehydrates from node_embeddings on restart
  - solov2-f8p: repo.Add records the HEAD branch (not '')
  - solov2-b36: eng_suppress_finding rejects unknown finding_ids
"""

from __future__ import annotations

import time

import pytest

from tests.mcp.helpers import query, scalar


def test_critical_promoted_sha_matches_head(mcp_client, repo_id):
    """repos.last_promoted_sha must exist and look like a SHA. A NULL or
    empty value means the c47 fix regressed and the daemon will treat
    this repo as never-promoted on the next restart."""
    row = query("SELECT last_promoted_sha FROM repos WHERE repo_id = ?", (repo_id,))
    assert row, f"repo {repo_id} missing from repos table"
    sha = row[0]["last_promoted_sha"]
    assert sha, "last_promoted_sha is NULL/empty — c47 regression"
    assert len(sha) == 40, f"last_promoted_sha is not a git SHA: {sha!r}"


def test_critical_active_branch_is_set(repo_id):
    """repos.active_branch must be set for queryability .
    Every node write keys by this branch; a NULL would silently route
    edits into a wrong-branch staging that never promotes."""
    branch = scalar("SELECT active_branch FROM repos WHERE repo_id = ?", (repo_id,))
    assert branch, "active_branch is NULL/empty — f8p regression"


def test_critical_nodes_carry_snippet(repo_id, branch):
    """sqlite.PromotionStore must bind nodes.snippet so embed-text has
    body to work with . At least one function node should
    have a non-NULL snippet."""
    n = scalar(
        """SELECT COUNT(*) FROM nodes
           WHERE repo_id = ? AND branch = ? AND kind = 'function'
                 AND snippet IS NOT NULL AND snippet <> ''""",
        (repo_id, branch),
    )
    assert n is not None and n > 0, (
        "no function node has a snippet — sxa regressed; embed text will degrade to "
        "kind+name+path only and semantic search quality will tank"
    )


def test_critical_vector_store_serves_ready_refs(mcp_client, repo_id, branch):
    """The vector store rehydrated from node_embeddings  must
    actually serve queries. eng_search_semantic caps k at 100 server-side
    (maxSearchK in tools_search.go) so we can't introspect the full vector
    population through the search API — instead we assert non-zero hits
    plus that 'scans_in_flight' is empty (otherwise the snapshot is mid-
    rehydrate and the count would be racy).

    The full 'every ready ref ⇒ vector exists' invariant lives in the
    in-process Go test TestDaemon_VectorStoreRehydratesOnSecondStart;
    this harness sibling verifies the surface behaves at all."""
    ready_count = scalar(
        """SELECT COUNT(*) FROM node_embedding_refs r
           JOIN nodes n ON n.node_id = r.node_id
           WHERE n.repo_id = ? AND n.branch = ? AND r.state = 'ready'""",
        (repo_id, branch),
    )
    assert ready_count and ready_count > 0, "no ready refs — populate the repo first"

    # Quiescence check — bail rather than race a mid-rehydrate snapshot.
    _, _, _, status = mcp_client.call("eng_get_status", {})
    if status.get("scans_in_flight"):
        pytest.skip(f"scan in flight: {status['scans_in_flight']}")

    ok, _, _, result = mcp_client.call("eng_search_semantic", {
        "repo_id": repo_id, "branch": branch,
        "query": "any text at all", "k": 50,
    })
    assert ok
    hits = result.get("results") or []
    assert len(hits) > 0, (
        f"vector store returned 0 hits despite {ready_count} ready refs — "
        "249 regression (rehydration dropped all rows) OR embedder hasn't run yet"
    )


def test_critical_promote_advances_sha(mcp_client, repo_id):
    """Calling eng_promote_repo twice in a row must keep last_promoted_sha
    pinned at HEAD. If the second call ZEROs the SHA or moves it backwards,
    c47's atomic-advance contract regressed."""
    root = query("SELECT root_path FROM repos WHERE repo_id = ?", (repo_id,))[0]["root_path"]

    ok1, _, _, r1 = mcp_client.call("eng_promote_repo", {"root_path": root})
    assert ok1
    sha1 = r1.get("git_sha")
    assert sha1

    ok2, _, _, r2 = mcp_client.call("eng_promote_repo", {"root_path": root})
    assert ok2
    sha2 = r2.get("git_sha")
    assert sha2 == sha1, f"second promote moved SHA: {sha1} → {sha2}"

    db_sha = scalar("SELECT last_promoted_sha FROM repos WHERE repo_id = ?", (repo_id,))
    assert db_sha == sha1, f"db sha {db_sha} != reported sha {sha1}"


def test_critical_suppress_unknown_rejected(mcp_client, repo_id, branch):
    """solov2-b36: orphan suppressions must not be creatable. If this
    accepts an arbitrary finding_id, list_suppressions will accumulate
    rows that point at nothing — once visible they survive forever."""
    sentinel = f"definitely-not-real-{int(time.time())}"
    ok, text, _, _ = mcp_client.call("eng_suppress_finding", {
        "repo_id": repo_id, "branch": branch,
        "finding_id": sentinel,
        "reason": "critical-path probe",
    })
    assert not ok, f"unknown finding_id was accepted — b36 regressed: {text}"
    # And no row should have been inserted with that sentinel.
    n = scalar("SELECT COUNT(*) FROM suppressions WHERE target = ?", (sentinel,))
    assert n == 0, f"orphan suppression row created for sentinel {sentinel!r}"


def test_critical_close_then_reopen_finding_is_round_trip(mcp_client, repo_id, branch):
    """Closing then reopening a finding must leave its state at 'open'.
    If close-followed-by-reopen left state='closed', findings could be
    silently lost from list_findings(state=open) — a sad failure mode
    for any agent relying on the open-set as actionable work."""
    fid = scalar(
        """SELECT finding_id FROM findings
           WHERE repo_id = ? AND branch = ? AND state = 'open' LIMIT 1""",
        (repo_id, branch),
    )
    if not fid:
        pytest.skip("no open finding — promote first to generate one")

    mcp_client.call("eng_close_finding", {
        "repo_id": repo_id, "branch": branch,
        "finding_id": fid, "reason": "critical-path round-trip",
    })
    mcp_client.call("eng_reopen_finding", {
        "repo_id": repo_id, "branch": branch, "finding_id": fid,
    })
    state = scalar("SELECT state FROM findings WHERE finding_id = ? AND branch = ?", (fid, branch))
    assert state == "open", f"close→reopen left state {state!r}, want 'open'"
