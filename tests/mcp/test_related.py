"""Tests for eng_find_related - symbols semantically similar to the code at
a given (file_path, line). Read-only; the (file_path, line) is derived from a
real promoted node so the cursor lands inside known source."""

from __future__ import annotations

import pytest

from tests.mcp.helpers import query


@pytest.fixture
def file_and_line(repo_id, branch) -> tuple[str, int]:
    """A (file_path, line) pair that sits on a promoted node, so
    eng_find_related has real code under the cursor to embed against."""
    rows = query(
        """SELECT file_path, line_start FROM nodes
           WHERE repo_id = ? AND branch = ? AND line_start IS NOT NULL
           ORDER BY (COALESCE(line_end,0) - COALESCE(line_start,0)) DESC
           LIMIT 1""",
        (repo_id, branch),
    )
    if not rows:
        pytest.skip("no promoted node with a line number - reindex first")
    return rows[0]["file_path"], rows[0]["line_start"]


def test_find_related_responds(mcp_client, repo_id, branch, file_and_line):
    file_path, line = file_and_line
    ok, text, _, result = mcp_client.call("eng_find_related", {
        "repo_id": repo_id, "branch": branch,
        "file_path": file_path, "line": line,
    })
    assert ok, f"eng_find_related failed: {text}"
    assert isinstance(result, dict)


def test_find_related_requires_file_path_and_line(mcp_client, repo_id, branch):
    """file_path and line are both required - omitting them must error,
    not silently return everything."""
    ok, text, _, _ = mcp_client.call("eng_find_related", {
        "repo_id": repo_id, "branch": branch,
    })
    assert not ok, "expected a required-param error"
    assert "method not found" not in text.lower()


def test_find_related_honors_k(mcp_client, repo_id, branch, file_and_line):
    """The result count must not exceed the requested k, and each hit must
    carry the ranked-result shape (node_id + score). Pinned to the real
    'results' key so a shape drift fails loudly rather than passing on an
    empty-because-wrong-key list."""
    file_path, line = file_and_line
    ok, text, _, result = mcp_client.call("eng_find_related", {
        "repo_id": repo_id, "branch": branch,
        "file_path": file_path, "line": line, "k": 3,
    })
    assert ok, f"eng_find_related k=3 failed: {text}"
    assert "results" in result, f"missing 'results' key - shape drift: {list(result)}"
    hits = result["results"]
    assert len(hits) <= 3, f"k=3 returned {len(hits)} hits"
    for h in hits:
        assert "node_id" in h and "score" in h, f"result missing node_id/score: {h}"
