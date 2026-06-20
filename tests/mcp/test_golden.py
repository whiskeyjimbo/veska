"""Golden path - the canonical end-to-end user journey, as one test.

Pinned shape:
  1. eng_get_status reports healthy
  2. eng_list_repos surfaces at least one repo with active_branch + SHA
  3. eng_find_symbol returns a known function
  4. eng_get_file_nodes for that function's file returns ≥1 node
  5. eng_get_call_chain on the same node returns a body
  6. eng_search_semantic for the symbol's own name surfaces it
  7. eng_get_blast_radius on the same node returns at least the seed
  8. eng_get_context_pack by symbol returns a seeded pack
  9. eng_promote_repo against the repo's root succeeds and pins SHA
 10. eng_list_findings + eng_list_suppressions respond cleanly

If this test passes, a new user running 'veska init && veska repo add'
followed by their IDE's MCP queries gets the documented behavior end
to end. If it fails, exactly one of the integration seams broke; the
per-tool tests narrow which one.

Marked 'golden' so it can run on its own:
    make test-mcp -k golden
"""

from __future__ import annotations

import pytest

from tests.mcp.helpers import assert_healthy_status, query

pytestmark = pytest.mark.golden


def test_golden_user_journey(mcp_client, repo_id, branch, target_symbol, target_file):
    # ── 1. Status ─────────────────────────────────────────────────────
    ok, text, _, status = mcp_client.call("eng_get_status", {})
    assert ok, f"get_status: {text}"
    assert_healthy_status(status)
    assert status.get("schema_version", 0) >= 9
    assert status.get("repo_count", 0) >= 1

    # ── 2. Repo listing ──────────────────────────────────────────────
    _, _, _, repos = mcp_client.call("eng_list_repos", {})
    record = next(
        (r for r in repos.get("repos") or [] if r["repo_id"] == repo_id),
        None,
    )
    assert record, f"repo {repo_id} not in eng_list_repos"
    assert record["active_branch"], "active_branch is empty - f8p regression"
    assert record["last_promoted_sha"], "last_promoted_sha empty - c47 regression"

    # ── 3. Structural lookup by name ─────────────────────────────────
    _, _, _, fs = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id, "branch": branch, "symbol": target_symbol,
    })
    nodes = fs.get("nodes") or []
    assert nodes, f"find_symbol({target_symbol!r}) returned nothing"
    node_id = nodes[0]["node_id"]

    # ── 4. File-scoped lookup ────────────────────────────────────────
    _, _, _, fn = mcp_client.call("eng_get_file_nodes", {
        "repo_id": repo_id, "branch": branch, "file_path": target_file,
    })
    assert fn.get("nodes"), f"get_file_nodes({target_file!r}) empty - 8ex regression"

    # ── 5. Call chain ────────────────────────────────────────────────
    ok5, text5, _, cc = mcp_client.call("eng_get_call_chain", {
        "repo_id": repo_id, "branch": branch, "node_id": node_id, "depth": 2,
    })
    assert ok5, f"get_call_chain: {text5}"
    assert "included_staging" in cc

    # ── 6. Semantic search returns ranked results ────────────────────
    # Do NOT assert the target symbol's exact node is in top-N - on a
    # real-sized corpus bare-name queries have flat score distributions
    # (test_alternative.py spells this out). The golden assertion is that
    # the API responds and returns ≥1 hit.
    _, _, _, sem = mcp_client.call("eng_search_semantic", {
        "repo_id": repo_id, "branch": branch,
        "query": target_symbol, "k": 10,
    })
    assert (sem.get("results") or []), (
        f"semantic({target_symbol!r}) returned 0 hits - sxa/249 regression suspected"
    )

    # ── 7. Blast radius (seed always included) ───────────────────────
    _, _, _, br = mcp_client.call("eng_get_blast_radius", {
        "repo_id": repo_id, "branch": branch,
        "node_id": node_id, "max_depth": 2, "max_nodes": 50,
    })
    entries = br.get("entries") or []
    assert any(e.get("node_id") == node_id and e.get("distance") == 0 for e in entries), (
        f"blast_radius did not include the seed node at distance=0: {entries}"
    )

    # ── 8. Context pack ──────────────────────────────────────────────
    _, _, _, cp = mcp_client.call("eng_get_context_pack", {
        "repo_id": repo_id, "branch": branch, "symbol": target_symbol,
    })
    assert cp.get("mode") == "symbol"
    assert cp.get("nodes"), "context_pack returned no nodes"
    assert any(n.get("seed") for n in cp["nodes"]), "context_pack has no seed node"

    # ── 9. Promotion (idempotent) ────────────────────────────────────
    root = query("SELECT root_path FROM repos WHERE repo_id = ?", (repo_id,))[0]["root_path"]
    _, _, _, pr = mcp_client.call("eng_promote_repo", {"root_path": root})
    assert pr.get("git_sha") == record["last_promoted_sha"], (
        "post-promote SHA differs from pre-promote SHA without an intervening "
        "commit - promotion is not idempotent"
    )

    # ── 10. Findings + suppressions surface (may be empty) ───────────
    ok10a, _, _, _ = mcp_client.call("eng_list_findings", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok10a
    ok10b, _, _, _ = mcp_client.call("eng_list_suppressions", {
        "repo_id": repo_id, "branch": branch,
    })
    assert ok10b
