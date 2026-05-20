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
