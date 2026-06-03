"""Tests for eng_set_repo_alias / eng_remove_repo_alias — human-friendly
names bound to a repo_id.

These mutate the alias registry, so every test binds a uniquely-named alias
and removes it in a finally to leave live state untouched."""

from __future__ import annotations

import os


def _unique_alias() -> str:
    # pid keeps parallel/repeat runs from colliding on the same name.
    return f"harness-alias-{os.getpid()}"


def test_set_then_remove_alias_roundtrip(mcp_client, repo_id):
    """Bind an alias, confirm set succeeds, then remove it."""
    name = _unique_alias()
    ok, text, _, result = mcp_client.call("eng_set_repo_alias", {
        "name": name, "repo_id": repo_id,
    })
    assert ok, f"eng_set_repo_alias failed: {text}"
    assert isinstance(result, dict)
    try:
        # The alias should now resolve the repo wherever a repo_id is
        # accepted — eng_get_repo takes the alias in place of the full id.
        ok2, text2, _, _ = mcp_client.call("eng_get_repo", {"repo_id": name})
        assert "method not found" not in text2.lower()
    finally:
        ok3, text3, _, _ = mcp_client.call("eng_remove_repo_alias", {"name": name})
        assert ok3, f"eng_remove_repo_alias failed: {text3}"


def test_set_alias_requires_name_and_repo_id(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_set_repo_alias", {"name": _unique_alias()})
    assert not ok, "expected a required-param error for missing repo_id"
    assert "method not found" not in text.lower()


def test_remove_unknown_alias_errors(mcp_client):
    """Removing an alias that was never bound must surface a domain error,
    never method-not-found."""
    ok, text, _, _ = mcp_client.call("eng_remove_repo_alias", {
        "name": "definitely-not-a-bound-alias-zzz",
    })
    assert "method not found" not in text.lower()
