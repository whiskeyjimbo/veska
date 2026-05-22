"""Tests for the graph MCP tools — find_symbol, get_file_nodes, get_node."""

from __future__ import annotations

import pytest

from tests.mcp.helpers import query


def test_find_symbol_returns_target(mcp_client, repo_id, branch, target_symbol):
    ok, text, _, result = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id,
        "branch": branch,
        "symbol": target_symbol,
    })
    assert ok, f"eng_find_symbol failed: {text}"
    names = [n.get("Name") for n in result.get("nodes", [])]
    assert target_symbol in names, f"{target_symbol!r} missing from {names}"


def test_find_symbol_unknown_is_empty(mcp_client, repo_id, branch):
    ok, _, _, result = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id,
        "branch": branch,
        "symbol": "zzz_definitely_not_a_symbol_99999",
    })
    assert ok
    assert result.get("nodes") in (None, []), f"want empty, got {result.get('nodes')}"


def test_find_symbol_requires_args(mcp_client, repo_id):
    ok, text, _, _ = mcp_client.call("eng_find_symbol", {"repo_id": repo_id})
    assert not ok and "required" in text.lower()


def test_get_file_nodes_returns_file_content(mcp_client, repo_id, branch, target_file):
    ok, text, _, result = mcp_client.call("eng_get_file_nodes", {
        "repo_id": repo_id,
        "branch": branch,
        "file_path": target_file,
    })
    assert ok, f"eng_get_file_nodes failed: {text}"
    nodes = result.get("nodes", [])
    assert nodes, "expected at least one node (solov2-8ex regression check)"
    # Every node returned must belong to the requested file.
    for n in nodes:
        assert n.get("Path") == target_file, f"node {n} has wrong file_path"


@pytest.mark.deep
def test_get_file_nodes_matches_sqlite(mcp_client, repo_id, branch, target_file):
    db_count = query(
        "SELECT COUNT(*) AS c FROM nodes WHERE repo_id = ? AND branch = ? AND file_path = ?",
        (repo_id, branch, target_file),
    )[0]["c"]
    _, _, _, result = mcp_client.call("eng_get_file_nodes", {
        "repo_id": repo_id,
        "branch": branch,
        "file_path": target_file,
    })
    mcp_count = len(result.get("nodes", []))
    assert mcp_count == db_count, f"MCP {mcp_count} != sqlite {db_count}"


def test_get_node_by_id(mcp_client, repo_id, branch, target_symbol):
    _, _, _, find_result = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id,
        "branch": branch,
        "symbol": target_symbol,
    })
    nodes = find_result.get("nodes", [])
    assert nodes, "find_symbol returned nothing — cannot test get_node"
    node_id = nodes[0]["node_id"]

    ok, text, _, result = mcp_client.call("eng_get_node", {
        "repo_id": repo_id,
        "branch": branch,
        "node_id": node_id,
    })
    assert ok, f"eng_get_node failed: {text}"
    # get_node returns a GraphResponse with a single-node nodes array.
    returned = result.get("nodes", [])
    assert returned, "expected one node"
    assert returned[0]["node_id"] == node_id
