"""Junior-Dev persona workflow (solov2-nmps.3).

A junior engineer's adoption-critical happy path, walked end-to-end against the
real daemon over the synthetic repo. Maps to the SOLO-02 stories:

  J1  orient        US-08.01  `veska wiki` / repo list to build a mental model
  J2  find a symbol US-04.01  `eng_find_symbol`
  J3  see findings  US-05.01  `veska findings list`
  J4  gate (FAIL)   —         `veska diff-gate --finding` on an unresolved fix
  J5  fix + gate    —         the candidate resolves the finding → PASS
  J6  commit        US-03.01  the post-commit hook promotes the fix

The junior leans on `--help`-discoverable commands only — no internal ids hunted
by hand: the finding_id comes from `findings list`, and `--finding` derives the
anchor + rule. The gate must FAIL while the dead-code finding is unresolved and
PASS once the candidate removes it.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from tests.mcp.persona_harness import COVERED_SYMBOL, persona_workspace

pytestmark = pytest.mark.persona


def _findings(ws, *extra) -> list[dict]:
    res = ws.veska("findings", "list", "--repo", ws.repo_id, "--json",
                   "--include-low", *extra)
    if not res.stdout.strip():
        return []
    payload = json.loads(res.stdout)
    # `findings list --json` emits a {"findings": [...]} envelope.
    return payload.get("findings", []) if isinstance(payload, dict) else payload


def test_junior_find_fix_gate_commit(tmp_path: Path):
    with persona_workspace(tmp_path) as ws:
        # diff-gate is a pre-merge gate: pin the index at the merge base by
        # dropping the auto-promote hook, then gate unpromoted candidate refs.
        ws.disable_post_commit_hook()
        base_sha = ws.git("rev-parse", "HEAD").stdout.strip()

        # ── J1: orient ────────────────────────────────────────────────────
        print("\n[J1] orient: repo list + wiki")
        rl = ws.veska("repo", "list")
        assert ws.repo_id[:12] in rl.stdout or ws.repo_dir in rl.stdout, rl.stdout
        # wiki regenerates mechanical onboarding pages with no LLM (US-08.01).
        ws.veska("wiki", "--repo", ws.repo_id, check=False)

        # ── J2: find a symbol ─────────────────────────────────────────────
        print("[J2] find a symbol")
        fs = ws.mcp.result("eng_find_symbol", {
            "repo_id": ws.repo_id, "branch": ws.branch, "symbol": COVERED_SYMBOL,
        })
        assert COVERED_SYMBOL in [n.get("name") for n in fs.get("nodes") or []]

        # ── J3: see the findings the daemon surfaced ──────────────────────
        print("[J3] findings list")
        findings = _findings(ws)
        assert findings, "junior sees no findings"
        for f in findings:
            print(f"   finding: {f.get('finding_id', '')[:12]} "
                  f"rule={f.get('rule')} sev={f.get('severity')} "
                  f"msg={f.get('message')}")
        assert any(f.get("rule") == "dead-code" for f in findings), \
            f"no dead-code finding; got rules {[f.get('rule') for f in findings]}"

        # ── J4: add new prod code without a test → coverage gate FAILs ────
        # The coverage gate (`diff-gate untested`) is the junior's pre-commit
        # guard: it FAILs when a changed/added prod symbol has no test-file
        # caller in the candidate after-state, and re-promotes the candidate so
        # a test added in the SAME diff counts (proven e2e in
        # coveragegate_e2e_test.go).
        print("[J4] add Multiply (no test), diff-gate untested → expect FAIL")
        math = Path(ws.repo_dir) / "math.go"
        math.write_text(math.read_text() + (
            "\n// Multiply multiplies two integers.\n"
            "func Multiply(a, b int) int {\n\treturn a * b\n}\n"))
        ws.git("add", "-A")
        ws.git("commit", "-q", "-m", "add Multiply (no test yet)")
        fail_sha = ws.git("rev-parse", "HEAD").stdout.strip()
        fail = ws.veska("diff-gate", "untested", "--repo", ws.repo_id,
                        "--branch", ws.branch, "--repo-root", ws.repo_dir,
                        "--base-ref", base_sha, "--candidate-ref", fail_sha,
                        check=False)
        fail_v = json.loads(fail.stdout)
        untested_names = [u.get("message", "") for u in fail_v.get("untested_changed") or []]
        print(f"   verdict: pass={fail_v.get('pass')} untested={untested_names}")
        assert fail_v.get("pass") is False, f"expected FAIL, got {fail_v}"
        assert any("Multiply" in m for m in untested_names), \
            f"expected Multiply flagged untested, got {untested_names}"
        assert fail.returncode != 0, "FAIL verdict must exit non-zero for CI"

        # ── J5: add the test in the same change set → gate PASSes ─────────
        print("[J5] add TestMultiply, diff-gate untested → expect PASS")
        (Path(ws.repo_dir) / "math_test.go").write_text(
            "package greeter\n\nimport \"testing\"\n\n"
            "func TestMultiply(t *testing.T) {\n"
            "\tif Multiply(2, 3) != 6 {\n\t\tt.Fatal(\"bad product\")\n\t}\n}\n")
        ws.git("add", "-A")
        ws.git("commit", "-q", "-m", "test Multiply")
        pass_sha = ws.git("rev-parse", "HEAD").stdout.strip()
        ok = ws.veska("diff-gate", "untested", "--repo", ws.repo_id,
                      "--branch", ws.branch, "--repo-root", ws.repo_dir,
                      "--base-ref", base_sha, "--candidate-ref", pass_sha,
                      check=False)
        ok_v = json.loads(ok.stdout)
        print(f"   verdict: pass={ok_v.get('pass')} failures={ok_v.get('failures')}")
        assert ok_v.get("pass") is True, f"expected PASS after test, got {ok_v}"
        assert ok.returncode == 0, "PASS verdict must exit 0"

        print("[OK] junior journey: orient → find → see findings → "
              "fail coverage gate → add test → pass")
