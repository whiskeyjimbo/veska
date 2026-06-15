"""Smoke test for the persona harness (solov2-nmps.1).

Proves the harness stands up a daemon + indexed synthetic repo and that the
promoted graph carries the four shapes every persona workflow relies on:
a CALLS edge, a test-covered symbol, an untested symbol, and an open finding.
If this is red, every junior/senior/agent test built on top is unreliable.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from tests.mcp.persona_harness import (
    CALLEE_SYMBOL,
    COVERED_SYMBOL,
    UNTESTED_SYMBOL,
    persona_workspace,
)

pytestmark = pytest.mark.persona


def test_harness_builds_realistic_graph(tmp_path: Path):
    with persona_workspace(tmp_path) as ws:
        print(f"\n[harness] repo_id={ws.repo_id[:12]} branch={ws.branch}")

        # Nodes promoted.
        n_nodes = ws.count("SELECT COUNT(*) FROM nodes WHERE repo_id=?", (ws.repo_id,))
        assert n_nodes > 0, "no nodes promoted"

        # The covered symbol is findable via MCP.
        fs = ws.mcp.result("eng_find_symbol", {
            "repo_id": ws.repo_id, "branch": ws.branch, "symbol": COVERED_SYMBOL,
        })
        names = [n.get("name") for n in fs.get("nodes") or []]
        assert COVERED_SYMBOL in names, f"find_symbol miss for {COVERED_SYMBOL}: {names}"

        # A real CALLS edge exists: GreetUser -> normalizeName.
        n_calls = ws.count(
            "SELECT COUNT(*) FROM edges WHERE repo_id=? AND kind='CALLS'", (ws.repo_id,))
        assert n_calls > 0, "no CALLS edges in the promoted graph"

        # The callee node exists (the CALLS edge resolves to a real symbol).
        callee = ws.mcp.result("eng_find_symbol", {
            "repo_id": ws.repo_id, "branch": ws.branch, "symbol": CALLEE_SYMBOL,
        })
        callee_names = [n.get("name") for n in callee.get("nodes") or []]
        assert CALLEE_SYMBOL in callee_names, f"callee {CALLEE_SYMBOL} missing"

        # The untested symbol is present (its finding is asserted via the gate
        # in the junior workflow; here we just confirm the symbol promoted).
        ut = ws.mcp.result("eng_find_symbol", {
            "repo_id": ws.repo_id, "branch": ws.branch, "symbol": UNTESTED_SYMBOL,
        })
        ut_names = [n.get("name") for n in ut.get("nodes") or []]
        assert UNTESTED_SYMBOL in ut_names, f"untested symbol {UNTESTED_SYMBOL} missing"

        # At least one open finding landed (dead-code and/or untested).
        finding_id = ws.open_finding_id()
        assert finding_id, "no open finding — structural checks did not fire"
        print(f"[harness] open finding: {finding_id}")
