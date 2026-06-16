"""Sad paths - every tool's documented error conditions, concentrated.

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

# Cases that don't depend on a real registered repo - purely testing
# that the handler rejects a missing required param. Each entry's params
# must omit *only* the param the want_substr refers to; supplying extra
# valid-looking placeholders (e.g. repo_id="x") risks the handler erroring
# on the placeholder before reaching the missing-arg check.
MISSING_REQUIRED_CASES = [
    # (method, params, expected_substring_in_error)
    ("eng_get_repo", {}, "required"),
    ("eng_add_repo", {}, "root_path"),
    ("eng_remove_repo", {}, "repo_id"),
    ("eng_promote_repo", {}, "root_path"),
    # node_id is always required for eng_get_node - repo_id/branch are now
    # optional , so the only remaining missing-arg path is
    # node_id itself.
    ("eng_get_node", {}, "required"),
    # find_symbol still requires `symbol`. Branch is optional 
    # and repo_id auto-resolves with one repo registered, so the only
    # missing-arg case left is the symbol itself.
    ("eng_find_symbol", {}, "required"),
    # call_chain now requires node_id OR symbol; supplying neither still
    # surfaces a "missing required" error .
    ("eng_get_call_chain", {}, "required"),
    ("eng_search_semantic", {}, "query"),
    ("eng_search_similar", {}, "node_id"),
    ("eng_get_blast_radius", {}, "node_id"),
    ("eng_get_dirty_blast_radius", {}, "required"),
    ("eng_get_diff_blast_radius", {}, "required"),
    # context_pack validates repo_id before symbol/task_id; the latter is
    # the "exactly one of" selector. Either substring is correct evidence
    # of a missing-arg rejection.
    ("eng_get_context_pack", {}, "required"),
    # find_owner validates repo_id before file_path and does NOT sole-repo
    # auto-resolve (unlike find_symbol), so an empty call is rejected on
    # repo_id first (solov2-khra: re-pinned from "file_path").
    ("eng_find_owner", {}, "repo_id"),
    # Findings family - finding_id is the always-required selector.
    ("eng_get_finding", {}, "finding_id"),
    ("eng_close_finding", {}, "finding_id"),
    ("eng_reopen_finding", {}, "finding_id"),
    ("eng_suppress_finding", {}, "finding_id"),
    ("eng_get_suppression", {}, "required"),
    ("eng_close_suppression", {}, "required"),
]


@pytest.mark.parametrize("method,params,want_substr", MISSING_REQUIRED_CASES)
def test_missing_required_args(mcp_client, method, params, want_substr):
    ok, text, _, _ = mcp_client.call(method, params)
    assert not ok, f"{method} unexpectedly succeeded with {params}"
    assert want_substr.lower() in text.lower(), (
        f"{method}({params}) error %r missing substring %r" % (text, want_substr)
    )


# Cases that need a real registered repo_id + file_path / branch to reach
# the missing-arg path. The fixture-backed test below supplies the real
# repo_id so the resolver succeeds and the missing field surfaces cleanly.
MISSING_REQUIRED_REPO_CASES = [
    # eng_get_file_nodes requires file_path; supplying real repo_id avoids
    # the resolver short-circuiting with "unknown repo_id".
    ("eng_get_file_nodes", "file_path"),
]


@pytest.mark.parametrize("method,want_substr", MISSING_REQUIRED_REPO_CASES)
def test_missing_required_args_with_real_repo(mcp_client, repo_id, branch, method, want_substr):
    ok, text, _, _ = mcp_client.call(method, {"repo_id": repo_id, "branch": branch})
    assert not ok, f"{method} unexpectedly succeeded"
    assert want_substr.lower() in text.lower(), (
        f"{method} error %r missing substring %r" % (text, want_substr)
    )


# ── Unknown identifiers ───────────────────────────────────────────────────────

UNKNOWN_ID_CASES = [
    # Tools that can prove non-existence and return a clear error.
    # repo_id="not-a-real-repo-zzz" intentionally bypasses the prefix
    # resolver (>= minRepoIDPrefix chars, no match) - surfaces the
    # explicit not-found path .
    ("eng_get_repo", {"repo_id": "not-a-real-repo-zzz"}, "not found"),
    ("eng_promote_repo", {"root_path": "/tmp/not-a-real-path-zzz"}, "not registered"),
]


@pytest.mark.parametrize("method,params,want_substr", UNKNOWN_ID_CASES)
def test_unknown_ids_loud(mcp_client, method, params, want_substr):
    ok, text, _, _ = mcp_client.call(method, params)
    assert not ok, f"{method}({params}) returned ok=True for an unknown id"
    assert want_substr.lower() in text.lower(), (
        f"{method} error %r missing substring %r" % (text, want_substr)
    )


def test_search_similar_unknown_node_is_loud(mcp_client, repo_id, branch):
    """eng_search_similar with a real repo but bogus node_id surfaces the
    shared node-id resolver error (-32002 'node_id … not in repo …') BEFORE
    it ever reaches the embedding lookup (solov2-izh6). The resolver
    rejecting the unknown id is the loud failure we want - the node simply
    doesn't exist in the repo (solov2-khra: re-pinned from the old
    'embedding'/'not found' wording)."""
    ok, text, _, _ = mcp_client.call("eng_search_similar", {
        "repo_id": repo_id,
        "branch": branch,
        "node_id": "deadbeef-zzz-no-such-node",
        "limit": 3,
    })
    assert not ok, "eng_search_similar unexpectedly succeeded for unknown node_id"
    assert "not in repo" in text.lower(), (
        f"eng_search_similar error %r missing 'not in repo'" % text
    )


# Tools that soft-fail on unknown ID (success + empty body). Pin the contract.
# eng_get_call_chain used to live here but now LOUDLY rejects an unknown
# node_id via the shared resolver (-32002) - see
# test_call_chain.py::test_call_chain_unknown_node_errors (solov2-khra).
SOFT_FAIL_UNKNOWN_CASES = [
    ("eng_find_symbol", "symbol"),       # unknown symbol → {nodes:nil}
]


@pytest.mark.parametrize("method,id_field", SOFT_FAIL_UNKNOWN_CASES)
def test_unknown_ids_soft_fail(mcp_client, repo_id, branch, method, id_field):
    params = {"repo_id": repo_id, "branch": branch, id_field: "definitely-not-real-zzz"}
    ok, _, _, result = mcp_client.call(method, params)
    assert ok, f"{method} unexpectedly errored on unknown {id_field}"
    nodes = result.get("nodes") if isinstance(result, dict) else None
    assert not nodes, f"{method} unexpectedly returned nodes for unknown {id_field}"


# ── Method-not-found surface ──────────────────────────────────────────────────


def test_method_not_found_for_nonsense_tool(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_not_a_real_tool_at_all", {})
    assert not ok
    assert "method not found" in text.lower()
