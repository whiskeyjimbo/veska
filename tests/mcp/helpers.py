"""Shared helpers for the veska MCP test suite.

These exist so individual tests can cross-validate MCP-tool results against
ground truth in SQLite without re-implementing the connection plumbing.
"""

from __future__ import annotations

import os
import sqlite3
from contextlib import contextmanager
from typing import Iterator


def veska_db_path() -> str:
    """Resolve the on-disk path of the veska SQLite database, honouring
    VESKA_HOME. Mirrors internal/config.veskaHome's default of ~/.veska."""
    home = os.environ.get("VESKA_HOME")
    if not home:
        home = os.path.expanduser("~/.veska")
    return os.path.join(home, "veska.db")


@contextmanager
def sqlite_ro() -> Iterator[sqlite3.Connection]:
    """Open the veska DB read-only. Read-only is enforced via the URI mode so
    the test process cannot accidentally corrupt the live daemon's writes."""
    path = veska_db_path()
    uri = f"file:{path}?mode=ro"
    conn = sqlite3.connect(uri, uri=True)
    try:
        yield conn
    finally:
        conn.close()


def query(sql: str, params: tuple = ()) -> list[dict]:
    """Run sql against the veska DB and return rows as dicts (column-keyed)."""
    with sqlite_ro() as c:
        c.row_factory = sqlite3.Row
        rows = c.execute(sql, params).fetchall()
        return [dict(r) for r in rows]


def scalar(sql: str, params: tuple = ()) -> object | None:
    """Return the first column of the first row of sql, or None when empty."""
    rows = query(sql, params)
    if not rows:
        return None
    first = rows[0]
    # dict insertion order matches SELECT order; first value is column 0
    return next(iter(first.values()))


def any_open_finding(repo_id: str, branch: str) -> str | None:
    """Return the id of one open finding for (repo_id, branch), or None.

    Shared by the findings and suppressions suites, which both need a live
    open finding to exercise their lifecycle round-trips. Keeping it here
    (rather than copied into each test file) means a findings-schema change
    touches one query, not two that can drift apart."""
    rows = query(
        """SELECT finding_id FROM findings
           WHERE repo_id = ? AND branch = ? AND state = 'open' LIMIT 1""",
        (repo_id, branch),
    )
    return rows[0]["finding_id"] if rows else None


def assert_healthy_status(status: dict) -> None:
    """Assert eng_get_status reflects a functioning daemon.

    These suites run against ONE shared session daemon, and sibling tests
    (var_decl, reindex, repo_lifecycle) register repos mid-run, so the
    embedder briefly carries a real backlog. eng_get_status emits exactly
    one degraded reason - 'embeddings_pending' (providers.go) - which the
    daemon itself classifies as "healthy, just warming up", so a transient
    'degraded' here is not a fault. We still fail loudly on any OTHER status
    or degraded reason, so the health gate survives: a genuinely broken
    daemon (or a new fault token) is not silently tolerated."""
    s = status.get("status")
    if s == "ok":
        return
    reasons = status.get("degraded_reasons") or []
    assert s == "degraded" and set(reasons) <= {"embeddings_pending"}, (
        f"unexpected status {s!r} with degraded_reasons={reasons!r} "
        "(only transient embeddings_pending is tolerated)"
    )
