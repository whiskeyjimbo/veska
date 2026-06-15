"""Agent persona workflow (solov2-nmps.5).

An AI agent drives veska over MCP to ground its reasoning in the graph and to
understand the blast radius of its in-flight (staged) edits — the live agent
loop. Maps to SOLO-02:

  A1  ground in context  US-08.02  eng_get_context_pack (symbol mode)
  A2  walk the call graph US-02.02  eng_get_call_chain (GreetUser → normalizeName)
  A3  staging-aware blast US-02.02  eng_get_dirty_blast_radius, included_staging

PARKED — task scoping. SOLO-02's task-anchored Agent stories (US-04.02,
US-09.02: eng_set_active_task / task-mode context_pack / eng_get_task_history)
are NOT reachable over the live MCP surface: RegisterTaskTools is parked off the
daemon registry (no MCP path to create a task — see tools_tasks.go), so those
calls return -32601. Filed as a persona-coverage finding under solov2-nmps; this
test asserts the agent loop that IS live and will grow a task-scoping phase when
the tools re-enable.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from tests.mcp.persona_harness import CALLEE_SYMBOL, COVERED_SYMBOL, persona_workspace

pytestmark = pytest.mark.persona


def test_agent_grounding_and_staging_aware(tmp_path: Path):
    with persona_workspace(tmp_path) as ws:
        # ── A1: ground reasoning in a symbol context pack ─────────────────
        print("\n[A1] context pack for GreetUser")
        pack = ws.mcp.result("eng_get_context_pack", {
            "repo_id": ws.repo_id, "branch": ws.branch, "symbol": COVERED_SYMBOL,
        })
        pack_names = {n.get("name") for n in pack.get("nodes") or []}
        print(f"   pack nodes={pack_names} tokens={pack.get('estimated_tokens')}"
              f"/{pack.get('token_budget')}")
        assert COVERED_SYMBOL in pack_names, f"seed missing from pack: {pack_names}"
        assert pack.get("token_budget"), "pack reports no token budget"

        # ── A2: walk the call graph ───────────────────────────────────────
        print("[A2] call chain from GreetUser")
        chain = ws.mcp.result("eng_get_call_chain", {
            "symbol": COVERED_SYMBOL, "repo_id": ws.repo_id, "branch": ws.branch,
            "depth": 3, "direction": "out",
        })
        chain_names = {n.get("name") for n in chain.get("nodes") or []}
        print(f"   chain nodes: {chain_names}")
        assert CALLEE_SYMBOL in chain_names, \
            f"callee {CALLEE_SYMBOL} not reached from {COVERED_SYMBOL}: {chain_names}"

        # ── A3: staging-aware blast radius of an in-flight edit ───────────
        print("[A3] edit (stage) GreetUser, dirty blast radius → included_staging")
        greeter = Path(ws.repo_dir) / "greeter.go"
        greeter.write_text(greeter.read_text().replace('"hello, "', '"hi there, "'))

        def _dirty():
            res = ws.mcp.result("eng_get_dirty_blast_radius", {
                "repo_id": ws.repo_id, "branch": ws.branch,
            })
            return res if res.get("included_staging") else None

        # fsnotify staging is asynchronous; poll until the overlay is observed.
        ws.wait(_dirty, 20, "dirty blast radius reflects the staged edit")
        dirty = _dirty()
        entries = {e.get("name") for e in dirty.get("entries") or []}
        print(f"   dirty entries={entries} included_staging={dirty.get('included_staging')}")
        assert dirty.get("included_staging") is True, "staging not reflected"

        print("[OK] agent journey: ground (pack) → call-chain → staging-aware blast")
