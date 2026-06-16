"""Session-scoped MCP fixtures for the veska MCP test suite.

Spawns one veska-mcp subprocess for the whole session; each test calls
mcp_client.call(<tool>, {...}) which pretty-prints the request + response
so a human running `pytest -v -s` gets a readable transcript suitable for
visual verification.

Veska's MCP server speaks a flat JSON-RPC protocol: the tool name IS the
JSON-RPC method (no `tools/call` wrapper). That's distinct from the
upstream MCP spec and from the engram codebase's harness, so this client
is intentionally simpler.
"""

from __future__ import annotations

import json
import os
import subprocess
import time
from pathlib import Path
from typing import Callable

import pytest

from tests.mcp.helpers import any_open_finding, query, scalar, veska_db_path


# ── pretty-print helpers ───────────────────────────────────────────────────────

_RESP_MAX = 1200  # chars before truncation in transcript output
_W = 64

# Transient JSON-RPC error substrings that warrant a retry (case-insensitive
# match against the error text). New SQLITE_BUSY-class patterns get added
# here without touching MCPClient.call - the predicate stays closed for
# modification, open for extension (solov2-seut).
_TRANSIENT_ERROR_PATTERNS = (
    "database is locked",
)


def _is_transient_error(text: str) -> bool:
    low = text.lower()
    return any(p in low for p in _TRANSIENT_ERROR_PATTERNS)


def _fmt_args(args: dict) -> str:
    parts = []
    for k, v in args.items():
        sv = str(v)
        if len(sv) > 60:
            sv = sv[:57] + "..."
        parts.append(f"{k}={sv!r}")
    return "  ".join(parts)


def _print_call(name: str, args: dict, ok: bool, text: str, elapsed: float) -> None:
    status = "✓" if ok else "✗"
    header = f"┌─ {name} {'─' * max(0, _W - len(name) - 3)}"
    print(f"\n{header}")
    if args:
        print(f"│  {_fmt_args(args)}")
    print(f"│  {'─' * _W}")
    body = text if len(text) <= _RESP_MAX else text[:_RESP_MAX] + f"\n… [{len(text)} chars total]"
    for line in body.splitlines():
        print(f"│  {line}")
    print(f"└─ {elapsed:.1f} ms  {status}")


# ── MCPClient ─────────────────────────────────────────────────────────────────


# A transcript sink is any callable with _print_call's signature. Injecting
# it (rather than calling _print_call directly inside MCPClient) keeps the
# client responsible only for JSON-RPC transport: changing the on-screen
# format, silencing it, or capturing it for assertions is now a constructor
# concern, not a reason to edit the transport class (solov2-gr3p).
Transcript = Callable[[str, dict, bool, str, float], None]


class MCPClient:
    """JSON-RPC-over-stdio wrapper for the veska-mcp shim.

    The shim forwards each line of stdin/stdout to/from the daemon's mcp.sock,
    so any io error usually means the daemon isn't running.

    Transport only: every call's request/response is handed to the injected
    ``transcript`` sink (default: the human-readable _print_call) so this
    class has a single reason to change - the JSON-RPC framing."""

    def __init__(self, binary: str = "./bin/veska-mcp", transcript: Transcript = _print_call):
        self._transcript = transcript
        env = os.environ.copy()
        self.proc = subprocess.Popen(
            [binary],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
            env=env,
        )
        self._id = 0

    def call(self, name: str, args: dict | None = None, retries: int = 3) -> tuple[bool, str, float, dict]:
        """Send one JSON-RPC call. Returns (ok, text, elapsed_ms, result_obj).

        Retries on transient SQLITE_BUSY: SQLite serialises writes through
        a single connection in the daemon's WriteHot pool, and back-to-back
        writes from rapid tests can collide with the embedder / queue
        / wiki workers that share the same lock. A 50ms backoff per attempt
        is plenty in practice.

        text is a pretty-printed JSON dump of the result (or error) for the
        on-screen transcript. result_obj is the parsed dict for assertions.
        """
        t0 = time.monotonic()
        last_text = ""
        last_err: dict = {}
        for attempt in range(retries):
            self._id += 1
            msg = {"jsonrpc": "2.0", "id": self._id, "method": name}
            if args is not None:
                msg["params"] = args
            try:
                self.proc.stdin.write(json.dumps(msg) + "\n")
                self.proc.stdin.flush()
                line = self.proc.stdout.readline()
            except (BrokenPipeError, ValueError) as e:
                elapsed = (time.monotonic() - t0) * 1000
                self._transcript(name, args or {}, False, f"<io-error: {e}>", elapsed)
                return False, str(e), elapsed, {}

            if not line:
                elapsed = (time.monotonic() - t0) * 1000
                self._transcript(name, args or {}, False, "<empty response>", elapsed)
                return False, "<empty response>", elapsed, {}

            resp = json.loads(line)
            if "error" in resp:
                last_text = json.dumps(resp["error"], indent=2)
                last_err = resp["error"]
                # Transient locking - retry without surfacing the failure
                # to the transcript so a successful retry reads cleanly.
                if _is_transient_error(last_text) and attempt < retries - 1:
                    time.sleep(0.05 * (attempt + 1))
                    continue
                elapsed = (time.monotonic() - t0) * 1000
                self._transcript(name, args or {}, False, last_text, elapsed)
                return False, last_text, elapsed, last_err

            result = resp.get("result", {})
            elapsed = (time.monotonic() - t0) * 1000
            text = json.dumps(result, indent=2)
            self._transcript(name, args or {}, True, text, elapsed)
            return True, text, elapsed, result if isinstance(result, dict) else {}

        # Exhausted retries - return the last error.
        elapsed = (time.monotonic() - t0) * 1000
        self._transcript(name, args or {}, False, last_text, elapsed)
        return False, last_text, elapsed, last_err

    def close(self) -> None:
        try:
            self.proc.stdin.close()
            self.proc.wait(timeout=5)
        except Exception:
            self.proc.kill()


# ── Fixtures ──────────────────────────────────────────────────────────────────


@pytest.fixture(scope="session")
def veska_home() -> str:
    home = os.environ.get("VESKA_HOME")
    if not home:
        pytest.skip("VESKA_HOME not set; export it to point at a running daemon's home")
    return home


@pytest.fixture(scope="session")
def mcp_client(veska_home):
    """One veska-mcp subprocess for the whole session. Skips when veska-mcp
    is missing or the daemon's mcp.sock isn't reachable."""
    binary = Path(os.environ.get("VESKA_BIN_DIR", "./bin")) / "veska-mcp"
    if not binary.exists():
        pytest.skip(f"{binary} not built - run `make build` first")

    client = MCPClient(str(binary))

    # Smoke-call eng_get_status. If it succeeds the rest of the session
    # can rely on the daemon being up. If it fails we skip the whole suite
    # cleanly rather than reporting N unrelated failures.
    ok, text, _, _ = client.call("eng_get_status", {})
    if not ok:
        client.close()
        pytest.skip(f"eng_get_status failed (daemon not running at {veska_home}?): {text}")

    yield client
    client.close()


@pytest.fixture(scope="session")
def repo_id(mcp_client) -> str:
    """Return a known repo_id with promoted content. Reads the live daemon's
    eng_list_repos and prefers the repo with the most promoted nodes - this
    avoids the trap of selecting an empty or orphaned entry (e.g. a stale
    row from a test that crashed before its eng_remove_repo cleanup ran).
    Skips when nothing is registered."""
    ok, _, _, result = mcp_client.call("eng_list_repos", {})
    if not ok:
        pytest.skip("eng_list_repos failed")
    repos = result.get("repos", []) if isinstance(result, dict) else []
    if not repos:
        pytest.skip("No repos registered - run `veska repo add <path>` first")

    # Rank by node count to skip empty/orphaned rows.
    best, best_n = None, -1
    for r in repos:
        rid = r["repo_id"]
        row = query("SELECT COUNT(*) AS c FROM nodes WHERE repo_id = ?", (rid,))
        n = row[0]["c"] if row else 0
        if n > best_n:
            best, best_n = rid, n
    if best_n <= 0:
        pytest.skip(
            "No repo has promoted nodes - run `veska reindex <path>` against a "
            "registered repo first"
        )
    return best


@pytest.fixture(scope="session")
def branch(repo_id) -> str:
    """Resolve repo_id's active_branch via direct SQLite query so MCP tests
    can pass the right branch to every parameter."""
    row = query("SELECT active_branch FROM repos WHERE repo_id = ?", (repo_id,))
    if not row or not row[0]["active_branch"]:
        return "main"
    return row[0]["active_branch"]


@pytest.fixture(scope="session")
def target_symbol(repo_id, branch) -> str:
    """A symbol guaranteed to exist in the repo's promoted graph - picks the
    most line-spanning function so cross-validation queries have something
    meaningful to compare against."""
    rows = query(
        """
        SELECT symbol_path FROM nodes
        WHERE repo_id = ? AND branch = ? AND kind = 'function'
        ORDER BY (COALESCE(line_end,0) - COALESCE(line_start,0)) DESC, symbol_path
        LIMIT 1
        """,
        (repo_id, branch),
    )
    if not rows:
        # Fall back to any node - useful for non-Go fixtures.
        rows = query(
            "SELECT symbol_path FROM nodes WHERE repo_id = ? AND branch = ? LIMIT 1",
            (repo_id, branch),
        )
    if not rows:
        pytest.skip("Repo has no promoted nodes - run `veska reindex` first")
    return rows[0]["symbol_path"]


@pytest.fixture(scope="session")
def target_file(repo_id, branch) -> str:
    """A file_path that has at least one promoted node for the repo."""
    rows = query(
        "SELECT DISTINCT file_path FROM nodes WHERE repo_id = ? AND branch = ? LIMIT 1",
        (repo_id, branch),
    )
    if not rows:
        pytest.skip("Repo has no promoted files")
    return rows[0]["file_path"]


@pytest.fixture(scope="session")
def fixture_summary(repo_id, branch):
    """Print a one-line summary of what we're testing against so the
    transcript header is self-explanatory."""
    n_nodes = scalar(
        "SELECT COUNT(*) FROM nodes WHERE repo_id = ? AND branch = ?",
        (repo_id, branch),
    )
    n_embeddings = scalar("SELECT COUNT(*) FROM node_embeddings") or 0
    sha = scalar("SELECT last_promoted_sha FROM repos WHERE repo_id = ?", (repo_id,))
    print(
        f"\n┌─ fixture ────────────────────────────────────────────────────\n"
        f"│  db:         {veska_db_path()}\n"
        f"│  repo_id:    {repo_id}\n"
        f"│  branch:     {branch}\n"
        f"│  nodes:      {n_nodes}\n"
        f"│  embeddings: {n_embeddings}\n"
        f"│  HEAD@last:  {sha}\n"
        f"└──────────────────────────────────────────────────────────────"
    )
    return {"repo_id": repo_id, "branch": branch, "nodes": n_nodes}


@pytest.fixture
def open_finding(repo_id, branch) -> str:
    """Id of one open finding for the active repo/branch. Shared by the
    findings and suppressions suites - both need a live finding to drive
    their lifecycle round-trips. Skips when none exist (promote first)."""
    fid = any_open_finding(repo_id, branch)
    if not fid:
        pytest.skip("no open finding to exercise - promote first")
    return fid
