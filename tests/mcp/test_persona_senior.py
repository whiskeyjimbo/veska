"""Senior-Dev persona workflow.

A senior engineer exercises the depth a junior doesn't: structural onboarding,
blast-radius / context-pack reasoning, suppression governance, and recovery
across a daemon restart:

  S1  structural onboard   US-08.01  entry_points / hot_zone
  S2  blast radius         US-04.02  eng_get_blast_radius over a real edge
  S3  context pack         US-08.02  eng_get_context_pack (token-bounded)
  S4  suppression govern   US-05.02  suppress → list round-trip, persists
  S5  restart recovery     US-07.01  promoted state + suppression survive restart
"""

from __future__ import annotations

from pathlib import Path

import pytest

from tests.mcp.persona_harness import CALLEE_SYMBOL, COVERED_SYMBOL, persona_workspace

pytestmark = pytest.mark.persona


def _node_id(ws, symbol: str) -> str:
    res = ws.mcp.result("eng_find_symbol", {
        "repo_id": ws.repo_id, "branch": ws.branch, "symbol": symbol,
    })
    nodes = res.get("nodes") or []
    match = [n for n in nodes if n.get("name") == symbol]
    assert match, f"{symbol} not found: {nodes}"
    return match[0]["node_id"]


def test_senior_depth_and_recovery(tmp_path: Path):
    with persona_workspace(tmp_path) as ws:
        # ── S1: structural onboarding ─────────────────────────────────────
        print("\n[S1] entry_points + hot_zone")
        ep = ws.mcp.result("eng_get_entry_points", {
            "repo_id": ws.repo_id, "branch": ws.branch,
        })
        # GreetUser (called by its test) is a natural high-fan-in entry point.
        ep_names = [e.get("symbol") or e.get("name") for e in ep.get("entry_points") or []]
        print(f"   entry points: {ep_names}")
        ws.mcp.result("eng_get_hot_zone", {"repo_id": ws.repo_id, "branch": ws.branch})

        # ── S2: blast radius over a real CALLS edge ───────────────────────
        # Blast radius is inbound by default ("who is affected if I change this
        # node" = its callers). GreetUser CALLS normalizeName, so changing
        # normalizeName must surface GreetUser as an affected caller.
        print("[S2] blast radius of normalizeName (expect its caller GreetUser)")
        callee_id = _node_id(ws, CALLEE_SYMBOL)
        br = ws.mcp.result("eng_get_blast_radius", {
            "node_id": callee_id, "repo_id": ws.repo_id, "branch": ws.branch,
        })
        entries = br.get("entries") or []
        br_syms = {e.get("name") for e in entries}
        print(f"   blast entries: {br_syms}")
        assert entries, "blast radius empty for a node with a real inbound CALLS edge"
        assert COVERED_SYMBOL in br_syms, \
            f"caller {COVERED_SYMBOL} not in normalizeName blast radius: {br_syms}"

        # ── S3: context pack is the token-bounded neighbourhood ───────────
        print("[S3] context pack for GreetUser")
        pack = ws.mcp.result("eng_get_context_pack", {
            "repo_id": ws.repo_id, "branch": ws.branch, "symbol": COVERED_SYMBOL,
        })
        pack_names = {n.get("name") for n in pack.get("nodes") or []}
        print(f"   pack nodes: {pack_names} tokens={pack.get('estimated_tokens')}"
              f"/{pack.get('token_budget')}")
        assert COVERED_SYMBOL in pack_names, f"seed missing from pack: {pack_names}"
        assert CALLEE_SYMBOL in pack_names, f"callee missing from pack: {pack_names}"
        # Token-bounded: the pack reports a budget and either stays within it or
        # flags truncation (SOLO-12 §2 deterministic truncation).
        if pack.get("token_budget"):
            assert pack.get("estimated_tokens", 0) <= pack["token_budget"] \
                or pack.get("truncated"), "pack exceeded budget without truncation flag"

        # ── S4: suppression governance ────────────────────────────────────
        print("[S4] suppress a finding, verify list round-trip")
        target = ws.open_finding_id()
        assert target, "no open finding to govern"
        ws.mcp.result("eng_suppress_finding", {
            "finding_id": target, "scope": "finding",
            "reason": "accepted risk - senior review",
        })

        def _open_ids(include_suppressed: bool) -> set[str]:
            res = ws.mcp.result("eng_list_findings", {
                "repo_id": ws.repo_id, "branch": ws.branch,
                "include_suppressed": include_suppressed,
            })
            return {f.get("finding_id") for f in res.get("findings") or []}

        assert target not in _open_ids(False), "suppressed finding still listed by default"
        assert target in _open_ids(True), "suppressed finding hidden even with include_suppressed"

        # ── S5: restart recovery - state + suppression survive ────────────
        print("[S5] restart daemon, verify recovery")
        ws.restart_daemon()
        status = ws.mcp.result("eng_get_status")
        assert status.get("status") == "ok", f"status not ok after restart: {status}"
        # Promoted graph reloaded from sqlite.
        post = ws.mcp.result("eng_find_symbol", {
            "repo_id": ws.repo_id, "branch": ws.branch, "symbol": COVERED_SYMBOL,
        })
        assert COVERED_SYMBOL in [n.get("name") for n in post.get("nodes") or []], \
            "promoted state not recovered after restart"
        # Suppression persisted across the restart (SOLO-09: persists).
        assert target not in _open_ids(False), "suppression did not survive restart"
        assert target in _open_ids(True), "suppressed finding lost after restart"

        print("[OK] senior journey: onboard → blast → pack → suppress → restart-recover")
