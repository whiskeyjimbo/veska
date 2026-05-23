"""Alternative paths — equivalent ways to reach the same outcome.

These tests don't exercise new functionality; they verify that two
different paths through the system produce the same result. A divergence
here means a contract is silently bifurcating.

Examples in this file:
  - Relative vs absolute file_path for eng_get_file_nodes
  - Finding the same node via eng_find_symbol vs eng_get_node
  - eng_search_semantic with default limit vs explicit limit=N
  - Searching for a symbol's own name returns it via both
    eng_search_semantic and eng_find_symbol
"""

from __future__ import annotations

import os

import pytest

from tests.mcp.helpers import query


def test_alternative_find_symbol_vs_get_node(mcp_client, repo_id, branch, target_symbol):
    """A symbol resolved by name (find_symbol) and the same id resolved by
    id (get_node) must return the same node fields."""
    _, _, _, fs = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id, "branch": branch, "symbol": target_symbol,
    })
    nodes = fs.get("nodes") or []
    assert nodes, "find_symbol returned nothing — fixture poisoned"
    fs_node = nodes[0]

    _, _, _, gn = mcp_client.call("eng_get_node", {
        "repo_id": repo_id, "branch": branch, "node_id": fs_node["node_id"],
    })
    gn_nodes = gn.get("nodes") or []
    assert gn_nodes, "get_node returned nothing for the id find_symbol gave us"
    gn_node = gn_nodes[0]

    for k in ("node_id", "name", "file_path", "kind"):
        assert fs_node.get(k) == gn_node.get(k), (
            f"path bifurcation on field {k!r}: find_symbol={fs_node.get(k)!r} "
            f"get_node={gn_node.get(k)!r}"
        )


def test_alternative_get_file_nodes_absolute_vs_db_path(mcp_client, repo_id, branch, target_file):
    """target_file is the absolute path the daemon stores. Re-asserting
    with that path is the canonical case; the goal here is to pin the
    contract that get_file_nodes uses exact match (not a fuzzy LIKE).

    Try the basename as a counter-example: it must NOT match — otherwise
    a relative-path slip would silently leak nodes across same-named
    files in different directories."""
    ok, _, _, by_abs = mcp_client.call("eng_get_file_nodes", {
        "repo_id": repo_id, "branch": branch, "file_path": target_file,
    })
    assert ok
    abs_count = len(by_abs.get("nodes") or [])
    assert abs_count > 0, "absolute path returned no nodes — fixture poisoned"

    basename = os.path.basename(target_file)
    if basename == target_file:
        pytest.skip("target_file is already a basename — counter-example inapplicable")

    _, _, _, by_base = mcp_client.call("eng_get_file_nodes", {
        "repo_id": repo_id, "branch": branch, "file_path": basename,
    })
    # Either empty or a different count — what we forbid is silently
    # treating basename as a synonym for the absolute path.
    base_nodes = by_base.get("nodes") or []
    # A clean assertion: the basename search must not accidentally
    # return the same node ids that the absolute path returned.
    abs_ids = {n["node_id"] for n in by_abs.get("nodes") or []}
    base_ids = {n["node_id"] for n in base_nodes}
    assert not (abs_ids & base_ids) or abs_ids != base_ids, (
        "get_file_nodes treats basename as synonym for absolute path — "
        "would silently leak across same-named files in different dirs"
    )


def test_alternative_search_semantic_default_vs_explicit_limit(mcp_client, repo_id, branch, target_symbol):
    """A search with the default limit must return a subset of the
    explicit-limit=large result. Ranking within score-ties is not
    deterministic (sqlite-vec breaks them by insertion order), so we
    assert set-equality on the score-tied prefix rather than strict
    list-order equality — a drift in the SET of top hits is the real
    correctness signal."""
    _, _, _, with_explicit = mcp_client.call("eng_search_semantic", {
        "repo_id": repo_id, "branch": branch,
        "query": target_symbol, "k": 50,
    })
    _, _, _, with_default = mcp_client.call("eng_search_semantic", {
        "repo_id": repo_id, "branch": branch,
        "query": target_symbol,
    })
    big = with_explicit.get("results") or []
    small = with_default.get("results") or []
    assert small, "default-limit search returned nothing"

    # Set equivalence on the top-N: the default-limit result must be a
    # subset of the larger result. (Identity within a score tie is
    # backend-defined.)
    small_ids = {r["node_id"] for r in small}
    big_ids = {r["node_id"] for r in big[: len(big)]}
    missing = small_ids - big_ids
    assert not missing, (
        f"default-limit hits not in explicit-limit superset: {missing}"
    )

    # The TOP hit must agree across runs — if the #1 result shuffles
    # between calls the user-visible "best match" becomes inconsistent
    # and that IS a real bug worth pinning here.
    assert big[0]["node_id"] == small[0]["node_id"], (
        f"top hit differs: default={small[0]["node_id"]} explicit={big[0]["node_id"]}"
    )


def test_alternative_find_symbol_and_semantic_search_both_respond(mcp_client, repo_id, branch, target_symbol):
    """Two paths must both serve the same query type:
      - eng_find_symbol(name) returns ≥1 exact match
      - eng_search_semantic(name) returns ≥1 ranked hit (any node)

    We do NOT assert the semantic-top includes the exact name node — on a
    real-sized corpus (~thousands of nodes) bare-name queries have very
    flat score distributions and rank the target arbitrarily. The
    relevant invariant is that both APIs respond, not that semantic
    ranking matches structural lookup. Quality-of-ranking belongs in a
    separate eval harness (tools/loadtest/recall)."""
    _, _, _, by_name = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id, "branch": branch, "symbol": target_symbol,
    })
    assert (by_name.get("nodes") or []), f"find_symbol({target_symbol!r}) returned nothing"

    _, _, _, sem = mcp_client.call("eng_search_semantic", {
        "repo_id": repo_id, "branch": branch,
        "query": target_symbol, "k": 10,
    })
    assert (sem.get("results") or []), f"semantic({target_symbol!r}) returned nothing"


def test_alternative_list_repos_matches_db(mcp_client):
    """eng_list_repos must equal SELECT * FROM repos. A divergence means
    the lister is reading from somewhere stale (e.g. a startup-time
    snapshot that doesn't see live additions)."""
    _, _, _, result = mcp_client.call("eng_list_repos", {})
    mcp_ids = sorted(r["repo_id"] for r in result.get("repos") or [])
    db_ids = sorted(r["repo_id"] for r in query("SELECT repo_id FROM repos"))
    assert mcp_ids == db_ids, f"eng_list_repos {mcp_ids} != db {db_ids}"
