"""Committed persona-verify capture driver (solov2-nmps.8 follow-up).

The `/persona-verify` skill judges whether the MCP tools' real outputs match
what they *should* be. Previously the driver was improvised and thrown away each
run; this module makes it a repeatable artifact: it stands up the synthetic
fixture, enumerates the LIVE tool surface via `tools/list`, drives every tool
with realistic params (read tools + stateful lifecycle round-trips), and prints
every request/response VERBATIM for the model to judge.

It is a capture tool, not an assertion suite — it (almost) always passes; its
value is the `-s` transcript. It is marked `persona_verify` so it stays out of
`make test-persona`. Run it via:

    make persona-verify-capture
    # or: PYTHONPATH=. python3 -m pytest tests/mcp/persona_verify_driver.py -m persona_verify -s

The one thing it DOES assert: every live tool is either exercised here or
accounted for, so a newly-registered tool the driver lacks params for is
surfaced loudly (not silently skipped) — the model then judges it by hand and
the spec is extended.
"""

from __future__ import annotations

import json
import os
import sqlite3
import subprocess
from pathlib import Path

import pytest

from tests.mcp.persona_harness import persona_workspace

pytestmark = pytest.mark.persona_verify

_MAX = 1600  # chars before a response is truncated in the transcript


class Capture:
    """Drives a tool, records the verbatim request/response, never raises."""

    def __init__(self, mcp):
        self.mcp = mcp
        self.rows: list[dict] = []
        self.called: set[str] = set()

    def __call__(self, tool: str, params: dict, note: str = "") -> dict:
        self.called.add(tool)
        resp = self.mcp.call(tool, params)
        err = resp.get("error")
        result = resp.get("result")
        self.rows.append({
            "tool": tool, "params": params,
            "status": "rpc_error" if err else "ok",
            "response": err if err is not None else result,
            "note": note,
        })
        return (result or {}) if err is None else {}

    def dump(self) -> None:
        for r in self.rows:
            body = json.dumps(r["response"], default=str, indent=2)
            if len(body) > _MAX:
                body = body[:_MAX] + "\n  …<truncated>"
            flag = "✗" if r["status"] == "rpc_error" else "✓"
            note = f"  ({r['note']})" if r["note"] else ""
            print(f"\n┌─ {flag} {r['tool']}{note}")
            print(f"│  params: {json.dumps(r['params'], default=str)}")
            print(f"└─ response:\n{body}")


def _live_tools(mcp) -> list[str]:
    resp = mcp.call("tools/list", {})
    tools = (resp.get("result") or {}).get("tools") or []
    return sorted(t.get("name") for t in tools if t.get("name"))


def _second_git_repo(path: str) -> str:
    os.makedirs(path, exist_ok=True)
    for args in (["init", "-q", "-b", "main"],
                 ["config", "user.email", "v@example.invalid"],
                 ["config", "user.name", "verify"]):
        subprocess.run(["git", "-C", path, *args], check=True, capture_output=True)
    (Path(path) / "lib.go").write_text("package lib\n\nfunc Helper() int { return 7 }\n")
    subprocess.run(["git", "-C", path, "add", "-A"], check=True, capture_output=True)
    subprocess.run(["git", "-C", path, "commit", "-q", "-m", "init"], check=True, capture_output=True)
    return path


def test_persona_verify_capture(tmp_path: Path):
    with persona_workspace(tmp_path) as ws:
        r, b, mcp = ws.repo_id, ws.branch, ws.mcp
        cap = Capture(mcp)

        # Ground truth from the corpus.
        with sqlite3.connect(f"file:{ws.db_path}?mode=ro", uri=True) as c:
            nodes = {row[1]: row[0] for row in c.execute(
                "SELECT node_id, symbol_path FROM nodes WHERE repo_id=?", (r,)).fetchall()}
        greet = nodes.get("GreetUser")
        callee = nodes.get("normalizeName")
        gfile = ws.file("greeter.go")
        live = _live_tools(mcp)
        print(f"\n@@ live tools/list = {len(live)} | repo={r[:12]} branch={b}")
        print(f"@@ corpus nodes = {sorted(k for k in nodes if not k.startswith('chunk'))}")

        # ── reads (no state deps) ─────────────────────────────────────────
        cap("eng_list_repos", {})
        cap("eng_get_repo", {"repo_id": r})
        cap("eng_get_current_repo", {}, "cwd-resolved; harness cwd is not the repo")
        cap("eng_get_status", {})
        cap("eng_get_config", {})
        cap("eng_find_symbol", {"repo_id": r, "branch": b, "symbol": "GreetUser"})
        cap("eng_get_node", {"repo_id": r, "branch": b, "node_id": greet})
        cap("eng_get_file_nodes", {"repo_id": r, "branch": b, "file_path": gfile})
        cap("eng_find_changed_symbols", {"repo_id": r, "branch": b, "ref_a": "HEAD", "ref_b": "HEAD"})
        cap("eng_list_dependencies", {"repo_id": r, "branch": b})
        cap("eng_search_semantic", {"repo_id": r, "branch": b, "query": "greeting for a user", "limit": 5})
        cap("eng_search_similar", {"repo_id": r, "branch": b, "node_id": greet, "limit": 5})
        cap("eng_find_related", {"repo_id": r, "branch": b, "file_path": gfile, "line": 5})
        cap("eng_find_clones", {"repo_id": r, "branch": b})
        cap("eng_get_blast_radius", {"repo_id": r, "branch": b, "node_id": callee},
            "callee → expect caller GreetUser inbound")
        cap("eng_get_dirty_blast_radius", {"repo_id": r, "branch": b},
            "clean tree → expect included_staging=false (nmps.11)")
        cap("eng_get_diff_blast_radius", {"repo_id": r, "branch": b, "ref_a": "HEAD", "ref_b": "HEAD"})
        cap("eng_get_call_chain", {"repo_id": r, "branch": b, "symbol": "GreetUser", "depth": 3, "direction": "out"})
        cap("eng_get_context_pack", {"repo_id": r, "branch": b, "symbol": "GreetUser"})
        cap("eng_get_entry_points", {"repo_id": r, "branch": b})
        cap("eng_get_hot_zone", {"repo_id": r, "branch": b})
        cap("eng_find_todos", {"repo_id": r, "branch": b})
        cap("eng_find_owner", {"repo_id": r, "branch": b, "file_path": gfile})

        # ── findings + suppression lifecycle (stateful) ───────────────────
        fl = cap("eng_list_findings", {"repo_id": r, "branch": b, "include_suppressed": True})
        finds = fl.get("findings") or []
        fid = next((f["finding_id"] for f in finds if f.get("rule") == "dead-code"),
                   finds[0]["finding_id"] if finds else "")
        cap("eng_get_finding", {"repo_id": r, "branch": b, "finding_id": fid})
        sup = cap("eng_suppress_finding", {"finding_id": fid, "scope": "finding", "reason": "verify capture"})
        sid = sup.get("suppression_id", "")
        cap("eng_list_suppressions", {"repo_id": r, "branch": b})
        cap("eng_get_suppression", {"suppression_id": sid})
        cap("eng_close_suppression", {"suppression_id": sid})
        cap("eng_close_finding", {"finding_id": fid, "branch": b, "repo_id": r, "reason": "verify capture"})
        # reopen takes no reason (asymmetry with close_finding, which does).
        cap("eng_reopen_finding", {"finding_id": fid, "branch": b, "repo_id": r})

        # ── repo alias lifecycle ──────────────────────────────────────────
        cap("eng_set_repo_alias", {"repo_id": r, "name": "verify-alias"})
        cap("eng_remove_repo_alias", {"name": "verify-alias"})

        # ── promotion / reindex (idempotent on a settled repo) ────────────
        cap("eng_promote_repo", {"repo_id": r})
        cap("eng_reindex_repo", {"repo_id": r})

        # ── add/remove a SECOND repo (don't disturb the fixture) ──────────
        second = _second_git_repo(str(tmp_path / "repo2"))
        add = cap("eng_add_repo", {"root_path": second})
        second_id = add.get("repo_id") or ""
        if second_id:
            cap("eng_remove_repo", {"repo_id": second_id})

        # ── transcript + coverage reconciliation ──────────────────────────
        cap.dump()
        not_exercised = [t for t in live if t not in cap.called]
        not_live = sorted(cap.called - set(live) - {"tools/list"})
        print("\n" + "=" * 60)
        print(f"COVERAGE: {len(cap.called - {'tools/list'})}/{len(live)} live tools exercised")
        if not_exercised:
            print(f"NOT EXERCISED (live tools with no driver spec — judge by hand "
                  f"+ extend the driver): {not_exercised}")
        if not_live:
            print(f"DRIVER CALLED NON-LIVE TOOLS (parked/removed?): {not_live}")
        print("Parked (expected absent from tools/list): "
              "eng_set_active_task, eng_get_active_task, eng_get_task_history")

        # The only hard assertion: no live tool is silently skipped.
        assert not not_exercised, (
            f"driver lacks params for live tool(s) {not_exercised} — extend the "
            f"spec so the verify sweep stays complete as the surface grows")
