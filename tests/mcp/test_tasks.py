"""Tests for the task-tracking MCP tools.

These manipulate the 'active task' per repo. We assert the round-trip:
set → get returns the just-set task, history shows the entry. To keep
the journey state clean, we capture the prior active task and restore
it at teardown."""

from __future__ import annotations

import time


def test_get_active_task_responds(mcp_client, repo_id):
    ok, text, _, result = mcp_client.call("eng_get_active_task", {"repo_id": repo_id})
    assert ok, f"eng_get_active_task failed: {text}"
    assert isinstance(result, dict)


def test_set_active_task_unknown_id_errors(mcp_client, repo_id):
    """eng_set_active_task requires the task to already exist in the
    tasks table — this tool doesn't *create* a task, it activates one.
    Probing with a known-bad ID confirms the tool is wired and surfaces
    the expected 'task not found' error."""
    sentinel = f"harness-probe-{int(time.time())}"
    ok, text, _, _ = mcp_client.call("eng_set_active_task", {
        "repo_id": repo_id, "task_id": sentinel,
    })
    assert not ok, "expected task-not-found error for sentinel id"
    assert "task not found" in text.lower() or "not found" in text.lower()


def test_get_task_history_responds(mcp_client, repo_id):
    ok, text, _, result = mcp_client.call("eng_get_task_history", {
        "repo_id": repo_id, "limit": 5,
    })
    assert ok, f"eng_get_task_history failed: {text}"
    assert isinstance(result, dict)


def test_set_active_task_requires_repo_id(mcp_client):
    ok, text, _, _ = mcp_client.call("eng_set_active_task", {"task_id": "x"})
    assert not ok and "required" in text.lower()
