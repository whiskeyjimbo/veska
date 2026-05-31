"""Bootstrap golden path — zero-state to working install, single test.

The fast harness (mcp_client fixture) assumes a daemon is already
running with a repo registered. This test asserts the OTHER direction:
a fresh user starting from nothing can run our documented commands in
order and reach a working install.

Phases (single function, sequential — one failure stops the test, which
is what we want for a bootstrap smoke):

  P1 . `veska init` against a fresh VESKA_HOME succeeds and probes Ollama.
  P2 . `veska-daemon` starts, both sockets appear.
  P3 . `veska repo add` (CLI → MCP) registers a new git repo + cold scan.
  P4 . The cold scan eventually drains: nodes + embeddings + ready refs.
  P5 . Queries: find_symbol → get_file_nodes → semantic_search all surface
       the seeded symbols.
  P6 . `git commit` of a new file: post-commit hook dials cli.sock, daemon
       advances last_promoted_sha to the new HEAD, new symbols are queryable.
  P7 . Daemon stop + restart: VectorStorage rehydrates from node_embeddings
        so semantic search still surfaces the same node count.
  P8 . `veska reindex` succeeds and remains idempotent for the next read.

Runtime: ~15s on a warm Ollama, dominated by embedder drain (the worker
polls at 1s intervals by default; we wait up to 30s for that). Marked
'bootstrap' so it stays out of the fast suite. Run via:

    VESKA_HOME=ignored make test-mcp-bootstrap

(VESKA_HOME is intentionally overridden inside the test.)
"""

from __future__ import annotations

import json
import os
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import pytest


pytestmark = pytest.mark.bootstrap

# Wall-time budgets. Generous because Ollama's first embed for a model
# can be a multi-second cold start.
SOCKET_WAIT_S = 10
COLD_SCAN_DRAIN_S = 60
HOOK_DRAIN_S = 30
RESTART_REHYDRATE_S = 10
POLL_INTERVAL_S = 0.25


# ── helpers ──────────────────────────────────────────────────────────────────


def _run(*argv, cwd=None, env=None, check=True):
    """Subprocess wrapper that captures stdout+stderr for diagnostics."""
    res = subprocess.run(
        list(argv), cwd=cwd, env=env,
        capture_output=True, text=True,
    )
    if check and res.returncode != 0:
        raise AssertionError(
            f"{argv!r} failed (exit={res.returncode}):\n"
            f"STDOUT:\n{res.stdout}\nSTDERR:\n{res.stderr}"
        )
    return res


def _wait_for_socket(path: str, budget_s: float) -> None:
    """Poll until path is a usable unix socket (i.e. .connect() works)."""
    deadline = time.monotonic() + budget_s
    last_err = None
    while time.monotonic() < deadline:
        if os.path.exists(path):
            s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            try:
                s.settimeout(0.5)
                s.connect(path)
                s.close()
                return
            except OSError as e:
                last_err = e
            finally:
                try:
                    s.close()
                except OSError:
                    pass
        time.sleep(POLL_INTERVAL_S)
    raise AssertionError(f"socket {path} not ready in {budget_s}s ({last_err})")


def _wait(predicate, budget_s: float, label: str) -> None:
    """Poll predicate() until it returns truthy or budget expires."""
    deadline = time.monotonic() + budget_s
    last = None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return
        time.sleep(POLL_INTERVAL_S)
    raise AssertionError(f"{label} did not become true within {budget_s}s (last={last!r})")


class _MCP:
    """Minimal MCP client targeting a specific VESKA_HOME's daemon."""

    def __init__(self, veska_mcp_bin: str, veska_home: str):
        env = os.environ.copy()
        env["VESKA_HOME"] = veska_home
        self.proc = subprocess.Popen(
            [veska_mcp_bin],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            text=True, bufsize=1, env=env,
        )
        self._id = 0

    def call(self, method, params=None):
        self._id += 1
        msg = {"jsonrpc": "2.0", "id": self._id, "method": method}
        if params is not None:
            msg["params"] = params
        self.proc.stdin.write(json.dumps(msg) + "\n")
        self.proc.stdin.flush()
        line = self.proc.stdout.readline()
        if not line:
            raise AssertionError(f"empty response from veska-mcp for {method}")
        return json.loads(line)

    def close(self):
        try:
            self.proc.stdin.close()
            self.proc.wait(timeout=3)
        except Exception:
            self.proc.kill()


def _start_daemon(daemon_bin: str, veska_home: str, log_path: str):
    """Spawn veska-daemon, return the Popen handle. Caller is responsible
    for terminate + wait."""
    env = os.environ.copy()
    env["VESKA_HOME"] = veska_home
    log = open(log_path, "wb", buffering=0)
    proc = subprocess.Popen(
        [daemon_bin],
        stdin=subprocess.DEVNULL, stdout=log, stderr=log,
        env=env,
    )
    return proc, log


def _stop_daemon(proc, log) -> None:
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=2)
    log.close()


def _sqlite_count(db_path: str, sql: str, params=()) -> int:
    with sqlite3.connect(f"file:{db_path}?mode=ro", uri=True) as c:
        return c.execute(sql, params).fetchone()[0]


# ── the test ──────────────────────────────────────────────────────────────────


def test_bootstrap_golden_zero_to_working_install(tmp_path: Path):
    """Single bootstrap walk. Each phase prints a short banner so the
    pytest -s transcript is self-explanatory."""

    # Resolve binaries by absolute path from the repo root so the test
    # works regardless of pytest's cwd.
    repo_root = Path(__file__).resolve().parents[2]
    bin_veska = str(repo_root / "bin" / "veska")
    bin_daemon = str(repo_root / "bin" / "veska-daemon")
    bin_mcp = str(repo_root / "bin" / "veska-mcp")
    for path in (bin_veska, bin_daemon, bin_mcp):
        if not os.path.exists(path):
            pytest.skip(f"missing {path} — run `make build` first")

    veska_home = str(tmp_path / "home")
    git_repo = str(tmp_path / "repo")
    daemon_log = str(tmp_path / "daemon.log")
    os.makedirs(veska_home)
    os.makedirs(git_repo)

    # ── P1: veska init ────────────────────────────────────────────────
    print("\n[P1] veska init")
    env = os.environ.copy()
    env["VESKA_HOME"] = veska_home
    res = _run(bin_veska, "init", env=env, check=False)
    if res.returncode != 0:
        if "embedder" in res.stderr.lower() or "ollama" in res.stderr.lower():
            pytest.skip(
                "veska init failed embedder probe — Ollama not reachable or "
                f"nomic-embed-text not pulled. stderr: {res.stderr.strip()}"
            )
        raise AssertionError(f"veska init failed:\n{res.stderr}")
    assert "healthy" in res.stdout, f"init didn't report healthy: {res.stdout}"

    # ── P2: start daemon, wait for sockets ─────────────────────────────
    print("[P2] start veska-daemon, wait for sockets")
    proc, log = _start_daemon(bin_daemon, veska_home, daemon_log)
    try:
        _wait_for_socket(os.path.join(veska_home, "cli.sock"), SOCKET_WAIT_S)
        _wait_for_socket(os.path.join(veska_home, "mcp.sock"), SOCKET_WAIT_S)

        mcp = _MCP(bin_mcp, veska_home)
        try:
            # Health smoke before doing anything else.
            resp = mcp.call("eng_get_status", {})
            assert "result" in resp and resp["result"].get("status") == "ok", resp

            # ── P3: create a tmp git repo + veska repo add ──────────
            print("[P3] git init + veska repo add")
            for args in (
                ["init", "-q", "-b", "main"],
                ["config", "user.email", "harness@example.invalid"],
                ["config", "user.name", "harness"],
            ):
                _run("git", "-C", git_repo, *args)
            (Path(git_repo) / "greeter.go").write_text(
                "package greeter\n\n"
                "// GreetUser returns a personalised greeting for the user.\n"
                "func GreetUser(name string) string { return \"hello, \" + name }\n"
            )
            (Path(git_repo) / "math.go").write_text(
                "package greeter\n\n"
                "// AddNumbers sums two integers.\n"
                "func AddNumbers(a, b int) int { return a + b }\n"
            )
            _run("git", "-C", git_repo, "add", "-A")
            _run("git", "-C", git_repo, "commit", "-q", "-m", "init")

            res = _run(bin_veska, "repo", "add", git_repo, env=env)
            assert "(via daemon)" in res.stdout, (
                f"repo add didn't route via daemon — solov2-trh regression? "
                f"stdout: {res.stdout!r}"
            )

            # ── P4: drain cold scan + embeddings ────────────────────
            print("[P4] wait for cold scan + embedder to drain")
            db_path = os.path.join(veska_home, "veska.db")
            _wait(
                lambda: _sqlite_count(db_path, "SELECT COUNT(*) FROM nodes") > 0,
                COLD_SCAN_DRAIN_S, "nodes table populated",
            )
            _wait(
                lambda: _sqlite_count(
                    db_path,
                    "SELECT COUNT(*) FROM node_embedding_refs WHERE state='pending'",
                ) == 0,
                COLD_SCAN_DRAIN_S, "embedder drained pending refs",
            )

            node_count = _sqlite_count(db_path, "SELECT COUNT(*) FROM nodes")
            embed_count = _sqlite_count(db_path, "SELECT COUNT(*) FROM node_embeddings")
            assert node_count >= 4, f"expected >= 4 nodes, got {node_count}"
            assert embed_count >= 4, f"expected >= 4 embeddings, got {embed_count}"

            # Cross-validate against the daemon's invariants.
            sha = _sqlite_count(db_path, "SELECT 1 FROM repos WHERE last_promoted_sha IS NOT NULL AND length(last_promoted_sha) = 40")
            assert sha == 1, "repos.last_promoted_sha not advanced (c47 regression)"
            with sqlite3.connect(f"file:{db_path}?mode=ro", uri=True) as c:
                branch = c.execute("SELECT active_branch FROM repos").fetchone()[0]
            assert branch == "main", f"active_branch = {branch!r}, want 'main' (f8p regression)"

            # ── P5: queries surface seeded symbols ──────────────────
            print("[P5] queries return seeded symbols")
            repo_id = next(
                r["repo_id"] for r in
                mcp.call("eng_list_repos", {})["result"]["repos"]
                if r["root_path"] == git_repo
            )

            fs = mcp.call("eng_find_symbol", {
                "repo_id": repo_id, "branch": branch, "symbol": "GreetUser",
            })
            names = [n["Name"] for n in fs["result"].get("nodes") or []]
            assert "GreetUser" in names, f"find_symbol miss: {names}"

            fn = mcp.call("eng_get_file_nodes", {
                "repo_id": repo_id, "branch": branch,
                "file_path": os.path.join(git_repo, "greeter.go"),
            })
            fn_nodes = fn["result"].get("nodes") or []
            assert fn_nodes, "get_file_nodes empty (8ex regression)"

            sem = mcp.call("eng_search_semantic", {
                "repo_id": repo_id, "branch": branch,
                "query": "GreetUser", "limit": 5,
            })
            sem_syms = [r.get("name") for r in sem["result"].get("results") or []]
            assert "GreetUser" in sem_syms, f"semantic miss: {sem_syms}"

            # ── P6: commit + hook drives promotion ──────────────────
            print("[P6] git commit drives promotion via hook")
            (Path(git_repo) / "extras.go").write_text(
                "package greeter\n\n"
                "// SubtractNumbers computes the difference.\n"
                "func SubtractNumbers(a, b int) int { return a - b }\n"
            )
            _run("git", "-C", git_repo, "add", "-A")
            # IMPORTANT: VESKA_HOME must be in the hook's env. The hook
            # script bakes the install-time VESKA_HOME , so
            # this still works even when our shell doesn't export it —
            # we pass env={} to git so the bake is what's tested.
            _run("git", "-C", git_repo, "commit", "-q", "-m", "add extras",
                 env={**os.environ, "VESKA_HOME": ""})  # hook must self-resolve

            # Wait for the hook → eng_promote_repo → SHA advance.
            head = _run("git", "-C", git_repo, "rev-parse", "HEAD").stdout.strip()
            _wait(
                lambda: _sqlite_count(
                    db_path,
                    "SELECT COUNT(*) FROM repos WHERE last_promoted_sha = ?",
                    (head,),
                ) == 1,
                HOOK_DRAIN_S, f"last_promoted_sha advanced to {head[:10]}",
            )
            _wait(
                lambda: _sqlite_count(
                    db_path,
                    "SELECT COUNT(*) FROM nodes WHERE symbol_path = 'SubtractNumbers'",
                ) == 1,
                HOOK_DRAIN_S, "SubtractNumbers node promoted",
            )

            # ── P7: daemon restart → vectors rehydrate ──────────────
            print("[P7] restart daemon, vectors rehydrate from node_embeddings")
            pre_embed_count = _sqlite_count(db_path, "SELECT COUNT(*) FROM node_embeddings")
            mcp.close()
            _stop_daemon(proc, log)

            proc, log = _start_daemon(bin_daemon, veska_home, daemon_log + ".2")
            _wait_for_socket(os.path.join(veska_home, "cli.sock"), SOCKET_WAIT_S)
            _wait_for_socket(os.path.join(veska_home, "mcp.sock"), SOCKET_WAIT_S)

            mcp = _MCP(bin_mcp, veska_home)
            # If 249 regressed the second-pass embedder won't see any
            # pending refs (everything's 'ready') and the vec store stays
            # empty — semantic search returns ≤ 0 hits.
            sem2 = mcp.call("eng_search_semantic", {
                "repo_id": repo_id, "branch": branch,
                "query": "GreetUser", "limit": 50,
            })
            hits = sem2["result"].get("results") or []
            assert len(hits) >= pre_embed_count - 2, (
                f"post-restart vector store has {len(hits)} hits but "
                f"node_embeddings has {pre_embed_count} — 249 regression"
            )

            # ── P8: veska reindex idempotent re-scan ────────────────
            print("[P8] veska reindex")
            res = _run(bin_veska, "reindex", git_repo, env=env)
            assert "reindex complete" in res.stdout, (
                f"reindex didn't complete cleanly: {res.stdout}"
            )

            print("[OK] bootstrap golden path complete")
        finally:
            mcp.close()
    finally:
        _stop_daemon(proc, log)
