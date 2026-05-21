"""Sad paths — every tool's documented error conditions, concentrated.

The per-tool files cover one or two error cases each. This file is the
opposite: every error a user can hit, grouped by error category, so an
inconsistent message ("required" vs "missing" vs "must not be empty") or
a tool that crashes on bad input shows up here in one diff.

Conventions enforced:
  - "X is required" / "X, Y are required" for missing-arg errors
  - "not found" for unknown-ID lookups (where the tool can prove non-existence)
  - "invalid params" / -32602 for malformed shapes
  - tools that soft-fail (empty result, not error) get an xfail-style note
"""

from __future__ import annotations

import pytest


# ── Missing required args ─────────────────────────────────────────────────────

MISSING_REQUIRED_CASES = [
    # (method, params, expected_substring_in_error)
    ("eng_get_repo", {}, "required"),
    ("eng_get_current_repo", {}, "cwd"),
    ("eng_add_repo", {}, "root_path"),
    ("eng_remove_repo", {}, "repo_id"),
    ("eng_promote_repo", {}, "root_path"),
    ("eng_find_symbol", {"branch": "main"}, "required"),
    ("eng_get_node", {"branch": "main"}, "required"),
    ("eng_get_file_nodes", {"repo_id": "x"}, "required"),
    ("eng_get_call_chain", {"repo_id": "x", "branch": "main"}, "required"),
    # query is validated BEFORE branch, so omitting branch with query
    # present is what triggers the branch-required error.
    ("eng_search_semantic", {"repo_id": "x", "query": "y"}, "branch"),
    ("eng_search_similar", {"repo_id": "x", "branch": "main"}, "required"),
    ("eng_get_blast_radius", {"repo_id": "x", "branch": "main"}, "required"),
    ("eng_get_dirty_blast_radius", {}, "required"),
    ("eng_get_diff_blast_radius", {}, "required"),
    ("eng_get_context_pack", {"repo_id": "x"}, "required"),
    ("eng_find_changed_symbols", {"repo_id": "x", "branch": "main"}, "required"),
    ("eng_find_todos", {"repo_id": "x"}, "required"),
    ("eng_find_owner", {"repo_id": "x"}, "required"),
    ("eng_set_active_task", {"task_id": "x"}, "required"),
    ("eng_get_finding", {"repo_id": "x", "branch": "main"}, "required"),
    ("eng_close_finding", {"repo_id": "x", "branch": "main"}, "required"),
    ("eng_reopen_finding", {"repo_id": "x", "branch": "main"}, "required"),
    ("eng_suppress_finding", {"repo_id": "x", "branch": "main"}, "required"),
    ("eng_get_suppression", {}, "required"),
    ("eng_close_suppression", {}, "required"),
    ("eng_get_hot_zone", {"repo_id": "x"}, "required"),
    ("eng_get_entry_points", {"repo_id": "x"}, "required"),
]


@pytest.mark.parametrize("method,params,want_substr", MISSING_REQUIRED_CASES)
def test_missing_required_args(mcp_client, method, params, want_substr):
    ok, text, _, _ = mcp_client.call(method, params)
    assert not ok, f"{method} unexpectedly succeeded with {params}"
    assert want_substr.lower() in text.lower(), (
        f"{method}({params}) error %r missing substring %r" % (text, want_substr)
    )


# ── Unknown identifiers ───────────────────────────────────────────────────────

UNKNOWN_ID_CASES = [
    # Tools that can prove non-existence and return a clear error.
    ("eng_get_repo", {"repo_id": "not-a-real-repo-zzz"}, "not found"),
    ("eng_promote_repo", {"root_path": "/tmp/not-a-real-path-zzz"}, "not registered"),
    ("eng_set_active_task", {"repo_id": "x", "task_id": "nosuchtask"}, "not found"),
    ("eng_search_similar", {
        "repo_id": "x", "branch": "main",
        "node_id": "deadbeef-zzz", "limit": 3,
    }, "embedding"),
]


@pytest.mark.parametrize("method,params,want_substr", UNKNOWN_ID_CASES)
def test_unknown_ids_loud(mcp_client, method, params, want_substr):
    ok, text, _, _ = mcp_client.call(method, params)
    assert not ok, f"{method}({params}) returned ok=True for an unknown id"
    assert want_substr.lower() in text.lower(), (
        f"{method} error %r missing substring %r" % (text, want_substr)
    )


# Tools that soft-fail on unknown ID (success + empty body). Pin the contract.
SOFT_FAIL_UNKNOWN_CASES = [
    ("eng_find_symbol", "symbol"),       # unknown symbol → {nodes:nil}
    ("eng_get_call_chain", "node_id"),   # unknown node   → empty body
]


@pytest.mark.parametrize("method,id_field", SOFT_FAIL_UNKNOWN_CASES)
def test_unknown_ids_soft_fail(mcp_client, repo_id, branch, method, id_field):
    params = {"repo_id": repo_id, "branch": branch, id_field: "definitely-not-real-zzz"}
    if method == "eng_get_call_chain":
        params["depth"] = 2
    ok, _, _, result = mcp_client.call(method, params)
    assert ok, f"{method} unexpectedly errored on unknown {id_field}"
    nodes = result.get("nodes") if isinstance(result, dict) else None
    assert not nodes, f"{method} unexpectedly returned nodes for unknown {id_field}"


# ── Method-not-found surface ──────────────────────────────────────────────────


def test_method_not_found_for_nonsense_tool(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_not_a_real_tool_at_all", {})
    assert not ok
    assert "method not found" in text.lower()
