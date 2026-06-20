"""Self-contained persona-workflow harness.

Every persona test (junior / senior / agent) needs the same thing: a real
daemon running against a *realistic* code graph it can drive end-to-end. This
module gives them one reusable `persona_workspace()` context manager that

  1. runs `veska init` in a fresh tmp VESKA_HOME,
  2. starts `veska-daemon` and waits for both sockets,
  3. writes a synthetic-but-realistic Go repo, `git`-commits it, and
     `veska repo add`s it,
  4. drains the cold scan (nodes + embeddings + structural checks),

then yields a `PersonaWorkspace` with an MCP client, the resolved repo_id /
branch, and helpers for driving the CLI, git, and direct sqlite reads.

It deliberately reuses the daemon-spawn pattern proven by
`test_bootstrap_golden.py` (which stays untouched - a passing golden test is
not worth refactoring). The synthetic corpus is shaped so the promoted graph
carries, by construction:

  * a real CALLS edge          - GreetUser -> normalizeName
  * a test-covered function    - GreetUser, called by TestGreetUser
  * an untested function       - AddNumbers (exported, no test caller)
  * a dead-code finding source - staleHelper (unexported, uncalled)

so junior/senior/agent journeys have something true to query, gate, and
suppress. No Ollama required: `veska init` uses the baked-in model2vec
embedder by default.
"""

from __future__ import annotations

import contextlib
import json
import os
import socket
import sqlite3
import subprocess
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Iterator

import pytest

# ── wall-time budgets ──────────────────────────────────────────────────────────
# Generous: model2vec's first embed and the cold-scan drain dominate.
SOCKET_WAIT_S = 10
COLD_SCAN_DRAIN_S = 60
RESTART_REHYDRATE_S = 15
POLL_INTERVAL_S = 0.25


# ── synthetic corpus ───────────────────────────────────────────────────────────
# One package, four shapes, each load-bearing for a persona assertion. Keep the
# comments - they double as semantic-search bait and document intent.
_GREETER_GO = """package greeter

// GreetUser returns a personalised greeting for the user. Covered by
// TestGreetUser, and calls normalizeName (a real CALLS edge).
func GreetUser(name string) string {
\treturn "hello, " + normalizeName(name)
}

// normalizeName trims an empty display name to a fallback. Reached only via
// GreetUser, so it is NOT dead code.
func normalizeName(name string) string {
\tif name == "" {
\t\treturn "stranger"
\t}
\treturn name
}

// staleHelper is never referenced from anywhere - a deterministic dead-code
// finding source (unexported, uncalled, not main/init/Test*).
func staleHelper() string {
\treturn "unreachable"
}
"""

_MATH_GO = """package greeter

// AddNumbers sums two integers. Intentionally has no test caller, so it is a
// deterministic untested-symbol finding source.
func AddNumbers(a, b int) int {
\treturn a + b
}
"""

_GREETER_TEST_GO = """package greeter

import "testing"

// TestGreetUser gives GreetUser a test-file CALLS edge (covered-symbol proxy).
func TestGreetUser(t *testing.T) {
\tif GreetUser("ada") == "" {
\t\tt.Fatal("empty greeting")
\t}
}
"""

CORPUS: dict[str, str] = {
    "greeter.go": _GREETER_GO,
    "math.go": _MATH_GO,
    "greeter_test.go": _GREETER_TEST_GO,
}

# Symbols a persona test can rely on existing in the promoted graph.
COVERED_SYMBOL = "GreetUser"
UNTESTED_SYMBOL = "AddNumbers"
DEADCODE_SYMBOL = "staleHelper"
CALLEE_SYMBOL = "normalizeName"


# ── process / IO helpers (lifted from test_bootstrap_golden) ────────────────────


def resolve_binaries() -> dict[str, str]:
    """Absolute paths to the three binaries, or pytest.skip if unbuilt."""
    repo_root = Path(__file__).resolve().parents[2]
    bins = {
        "veska": str(repo_root / "bin" / "veska"),
        "daemon": str(repo_root / "bin" / "veska-daemon"),
        "mcp": str(repo_root / "bin" / "veska-mcp"),
    }
    for path in bins.values():
        if not os.path.exists(path):
            pytest.skip(f"missing {path} - run `make build` first")
    return bins


def run(*argv, cwd=None, env=None, check=True) -> subprocess.CompletedProcess:
    """Subprocess wrapper capturing stdout+stderr for diagnostics."""
    res = subprocess.run(list(argv), cwd=cwd, env=env, capture_output=True, text=True)
    if check and res.returncode != 0:
        raise AssertionError(
            f"{argv!r} failed (exit={res.returncode}):\n"
            f"STDOUT:\n{res.stdout}\nSTDERR:\n{res.stderr}"
        )
    return res


def wait_for_socket(path: str, budget_s: float) -> None:
    deadline = time.monotonic() + budget_s
    last_err = None
    while time.monotonic() < deadline:
        if os.path.exists(path):
            s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            try:
                s.settimeout(0.5)
                s.connect(path)
                return
            except OSError as e:
                last_err = e
            finally:
                with contextlib.suppress(OSError):
                    s.close()
        time.sleep(POLL_INTERVAL_S)
    raise AssertionError(f"socket {path} not ready in {budget_s}s ({last_err})")


def wait(predicate: Callable[[], object], budget_s: float, label: str) -> None:
    deadline = time.monotonic() + budget_s
    last = None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return
        time.sleep(POLL_INTERVAL_S)
    raise AssertionError(f"{label} did not become true within {budget_s}s (last={last!r})")


def sqlite_count(db_path: str, sql: str, params=()) -> int:
    with sqlite3.connect(f"file:{db_path}?mode=ro", uri=True) as c:
        return c.execute(sql, params).fetchone()[0]


class MCP:
    """Minimal MCP client targeting a specific VESKA_HOME's daemon.

    Veska speaks a flat JSON-RPC protocol - the tool name IS the method (no
    `tools/call` wrapper). `call()` returns the parsed JSON-RPC envelope;
    `result()` unwraps it and asserts no error.
    """

    def __init__(self, mcp_bin: str, veska_home: str):
        env = os.environ.copy()
        env["VESKA_HOME"] = veska_home
        self.proc = subprocess.Popen(
            [mcp_bin],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            text=True, bufsize=1, env=env,
        )
        self._id = 0

    def call(self, method: str, params: dict | None = None) -> dict:
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

    def result(self, method: str, params: dict | None = None) -> dict:
        resp = self.call(method, params)
        if "error" in resp and resp["error"] is not None:
            raise AssertionError(f"{method} returned error: {resp['error']}")
        return resp.get("result") or {}

    def close(self) -> None:
        try:
            self.proc.stdin.close()
            self.proc.wait(timeout=3)
        except Exception:
            self.proc.kill()


def _start_daemon(daemon_bin: str, veska_home: str, log_path: str):
    env = os.environ.copy()
    env["VESKA_HOME"] = veska_home
    log = open(log_path, "wb", buffering=0)
    proc = subprocess.Popen(
        [daemon_bin], stdin=subprocess.DEVNULL, stdout=log, stderr=log, env=env,
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


def write_synthetic_repo(repo_dir: str) -> None:
    """git-init `repo_dir`, write the synthetic corpus, and commit it."""
    os.makedirs(repo_dir, exist_ok=True)
    for args in (
        ["init", "-q", "-b", "main"],
        ["config", "user.email", "persona@example.invalid"],
        ["config", "user.name", "persona-harness"],
    ):
        run("git", "-C", repo_dir, *args)
    for name, body in CORPUS.items():
        (Path(repo_dir) / name).write_text(body)
    run("git", "-C", repo_dir, "add", "-A")
    run("git", "-C", repo_dir, "commit", "-q", "-m", "synthetic corpus")


# ── the workspace ───────────────────────────────────────────────────────────────


@dataclass
class PersonaWorkspace:
    """A live daemon + indexed synthetic repo, ready for a persona to drive."""

    bins: dict[str, str]
    home: str
    repo_dir: str
    db_path: str
    repo_id: str
    branch: str
    mcp: MCP
    _log_path: str
    _proc: object
    _log: object

    # - CLI / git drivers -
    def veska(self, *args, check=True) -> subprocess.CompletedProcess:
        env = os.environ.copy()
        env["VESKA_HOME"] = self.home
        return run(self.bins["veska"], *args, env=env, check=check)

    def git(self, *args, check=True) -> subprocess.CompletedProcess:
        return run("git", "-C", self.repo_dir, *args, check=check)

    def file(self, name: str) -> str:
        return os.path.join(self.repo_dir, name)

    # - direct-read helpers -
    def count(self, sql: str, params=()) -> int:
        return sqlite_count(self.db_path, sql, params)

    def wait(self, predicate, budget_s: float, label: str) -> None:
        wait(predicate, budget_s, label)

    def open_finding_id(self) -> str | None:
        with sqlite3.connect(f"file:{self.db_path}?mode=ro", uri=True) as c:
            row = c.execute(
                "SELECT finding_id FROM findings WHERE repo_id=? AND branch=? "
                "AND state='open' LIMIT 1",
                (self.repo_id, self.branch),
            ).fetchone()
        return row[0] if row else None

    def disable_post_commit_hook(self) -> None:
        """Remove the post-commit hook so candidate commits do NOT auto-promote.

        diff-gate is a pre-merge gate: in CI the index is pinned at the merge
        base and the candidate ref is unpromoted. The local post-commit hook
        would instead chase HEAD, advancing the index ahead of --base-ref (the
        N2 flow). Dropping the hook reproduces the CI/pre-merge invariant the
        gate is designed for.
        """
        hooks_dir = Path(self.repo_dir) / ".git" / "hooks"
        for hook in ("post-commit", "post-checkout"):
            with contextlib.suppress(FileNotFoundError):
                (hooks_dir / hook).unlink()

    def restart_daemon(self) -> None:
        """Stop and respawn the daemon, re-dialling a fresh MCP client.

        Exercises the SOLO-08 restart-recovery path: promoted state reloads
        from sqlite, the vector store rehydrates from node_embeddings.
        """
        self.mcp.close()
        _stop_daemon(self._proc, self._log)
        self._log_path = self._log_path + ".r"
        self._proc, self._log = _start_daemon(self.bins["daemon"], self.home, self._log_path)
        wait_for_socket(os.path.join(self.home, "cli.sock"), SOCKET_WAIT_S)
        wait_for_socket(os.path.join(self.home, "mcp.sock"), SOCKET_WAIT_S)
        self.mcp = MCP(self.bins["mcp"], self.home)


@contextlib.contextmanager
def persona_workspace(tmp_path: Path) -> Iterator[PersonaWorkspace]:
    """Stand up a daemon + indexed synthetic repo; tear it down on exit."""
    bins = resolve_binaries()
    home = str(tmp_path / "home")
    repo_dir = str(tmp_path / "repo")
    log_path = str(tmp_path / "daemon.log")
    os.makedirs(home)

    env = os.environ.copy()
    env["VESKA_HOME"] = home

    # P1 - init (model2vec by default; skip only if an embedder probe fails).
    res = run(bins["veska"], "init", "-y", env=env, check=False)
    if res.returncode != 0:
        if "embedder" in res.stderr.lower() or "ollama" in res.stderr.lower():
            pytest.skip(f"veska init failed embedder probe: {res.stderr.strip()}")
        raise AssertionError(f"veska init failed:\n{res.stderr}")

    proc, log = _start_daemon(bins["daemon"], home, log_path)
    mcp = None
    try:
        wait_for_socket(os.path.join(home, "cli.sock"), SOCKET_WAIT_S)
        wait_for_socket(os.path.join(home, "mcp.sock"), SOCKET_WAIT_S)

        # P3 - synthetic repo + repo add.
        write_synthetic_repo(repo_dir)
        run(bins["veska"], "repo", "add", repo_dir, env=env)

        # P4 - drain the cold scan: nodes promoted, embeddings + checks settled.
        db_path = os.path.join(home, "veska.db")
        wait(lambda: sqlite_count(db_path, "SELECT COUNT(*) FROM nodes") > 0,
             COLD_SCAN_DRAIN_S, "nodes populated")
        wait(lambda: sqlite_count(
                db_path,
                "SELECT COUNT(*) FROM node_embedding_refs WHERE state='pending'") == 0,
             COLD_SCAN_DRAIN_S, "embedder drained")
        # Structural checks (dead-code, untested) run post-promotion; wait for at
        # least one open finding so junior gating has a target.
        wait(lambda: sqlite_count(
                db_path, "SELECT COUNT(*) FROM findings WHERE state='open'") > 0,
             COLD_SCAN_DRAIN_S, "structural checks landed a finding")

        mcp = MCP(bins["mcp"], home)
        repos = mcp.result("eng_list_repos").get("repos") or []
        match = [r for r in repos if r.get("root_path") == repo_dir]
        if not match:
            raise AssertionError(f"repo {repo_dir} not registered; got {repos}")
        repo_id = match[0]["repo_id"]
        with sqlite3.connect(f"file:{db_path}?mode=ro", uri=True) as c:
            branch = c.execute("SELECT active_branch FROM repos WHERE repo_id=?",
                               (repo_id,)).fetchone()[0]

        ws = PersonaWorkspace(
            bins=bins, home=home, repo_dir=repo_dir, db_path=db_path,
            repo_id=repo_id, branch=branch, mcp=mcp,
            _log_path=log_path, _proc=proc, _log=log,
        )
        yield ws
        proc, log = ws._proc, ws._log  # restart_daemon may have swapped these
        mcp = ws.mcp
    finally:
        if mcp is not None:
            mcp.close()
        _stop_daemon(proc, log)
