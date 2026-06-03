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
    # Wire-shape keys are snake_case (internal/infrastructure/mcp/dto.go);
    # target_symbol is the symbol_path which maps to the 'name' field.
    names = [n.get("name") for n in result.get("nodes", [])]
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
        assert n.get("file_path") == target_file, f"node {n} has wrong file_path"


@pytest.mark.deep
def test_get_file_nodes_matches_sqlite(mcp_client, repo_id, branch, target_file):
    # eng_get_file_nodes filters out chunk:* pseudo-nodes (internal
    # file-fragment embedding units, nodesToDTO in dto.go), so the
    # ground-truth count must exclude them too or it over-counts every
    # chunked file (solov2-khra).
    db_count = query(
        "SELECT COUNT(*) AS c FROM nodes "
        "WHERE repo_id = ? AND branch = ? AND file_path = ? AND kind != 'chunk'",
        (repo_id, branch, target_file),
    )[0]["c"]
    _, _, _, result = mcp_client.call("eng_get_file_nodes", {
        "repo_id": repo_id,
        "branch": branch,
        "file_path": target_file,
    })
    mcp_count = len(result.get("nodes", []))
    assert mcp_count == db_count, f"MCP {mcp_count} != sqlite {db_count}"


def test_find_symbol_branch_defaults_to_active(mcp_client, repo_id, target_symbol):
    """solov2-5vu1: branch is optional and resolves to the registered
    active_branch. Omitting branch should return the same hits as
    supplying it explicitly."""
    ok, text, _, result = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id,
        "symbol": target_symbol,
    })
    assert ok, f"eng_find_symbol without branch should succeed, got: {text}"
    names = [n.get("name") for n in result.get("nodes", [])]
    assert target_symbol in names


def test_find_symbol_accepts_repo_id_prefix(mcp_client, repo_id, branch, target_symbol):
    """solov2-rkbc: any unambiguous repo_id prefix (>= 4 chars) resolves
    to the full id. Use an 8-char prefix — neither the full sha nor the
    canonical 12-char short_id — to prove the prefix path runs."""
    prefix = repo_id[:8]
    ok, text, _, result = mcp_client.call("eng_find_symbol", {
        "repo_id": prefix,
        "branch": branch,
        "symbol": target_symbol,
    })
    assert ok, f"8-char prefix should resolve, got: {text}"
    names = [n.get("name") for n in result.get("nodes", [])]
    assert target_symbol in names


def test_get_node_without_repo_id_or_branch(mcp_client, repo_id, branch, target_symbol):
    """solov2-v4ob: node_id is a content-hashed sha256 and globally unique,
    so eng_get_node must accept it without repo_id+branch."""
    _, _, _, find_result = mcp_client.call("eng_find_symbol", {
        "repo_id": repo_id, "branch": branch, "symbol": target_symbol,
    })
    nodes = find_result.get("nodes", [])
    assert nodes, "find_symbol returned nothing — cannot test get_node"
    node_id = nodes[0]["node_id"]

    ok, text, _, result = mcp_client.call("eng_get_node", {"node_id": node_id})
    assert ok, f"eng_get_node without repo_id/branch should succeed, got: {text}"
    returned = result.get("nodes", [])
    assert returned and returned[0]["node_id"] == node_id


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
