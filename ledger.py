"""Local sqlite3 ledger with content-hash idempotence and lock retries."""

from __future__ import annotations

import sqlite3
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


class LedgerError(RuntimeError):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def _connect(db_path: str | Path) -> sqlite3.Connection:
    path = Path(db_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(path, timeout=30.0, isolation_level=None)
    connection.execute("PRAGMA busy_timeout=30000")
    connection.execute("PRAGMA journal_mode=WAL")
    connection.execute("PRAGMA synchronous=NORMAL")
    return connection


def initialize(db_path: str | Path) -> None:
    connection = _connect(db_path)
    try:
        connection.execute(
            """CREATE TABLE IF NOT EXISTS runs (
                   content_hash TEXT PRIMARY KEY,
                   project TEXT NOT NULL,
                   status TEXT NOT NULL,
                   exit_code INTEGER NOT NULL,
                   reason TEXT,
                   payload TEXT NOT NULL,
                   updated_at TEXT NOT NULL
               )"""
        )
        connection.execute("CREATE INDEX IF NOT EXISTS runs_status_idx ON runs(status)")
    finally:
        connection.close()


def _with_retry(operation: Any, retries: int = 12) -> Any:
    last_error: Exception | None = None
    for attempt in range(retries):
        try:
            return operation()
        except sqlite3.OperationalError as exc:
            last_error = exc
            if "locked" not in str(exc).lower() and "busy" not in str(exc).lower():
                raise
            time.sleep(min(0.5, 0.01 * (2 ** attempt)))
    raise LedgerError("write_lock_retry_exhausted", f"sqlite write retries exhausted: {last_error}")


def record_run(db_path: str | Path, content_hash: str, project: str, status: str,
               exit_code: int, payload: str, reason: str | None = None) -> None:
    timestamp = datetime.now(timezone.utc).isoformat()

    def write() -> None:
        connection = _connect(db_path)
        try:
            connection.execute(
                """INSERT INTO runs(content_hash, project, status, exit_code, reason, payload, updated_at)
                   VALUES (?, ?, ?, ?, ?, ?, ?)
                   ON CONFLICT(content_hash) DO UPDATE SET
                     status=excluded.status, exit_code=excluded.exit_code,
                     reason=excluded.reason, payload=excluded.payload, updated_at=excluded.updated_at""",
                (content_hash, project, status, int(exit_code), reason, payload, timestamp),
            )
        finally:
            connection.close()

    _with_retry(write)


def get_run(db_path: str | Path, content_hash: str) -> dict[str, Any] | None:
    connection = _connect(db_path)
    try:
        row = connection.execute(
            "SELECT content_hash, project, status, exit_code, reason, payload, updated_at "
            "FROM runs WHERE content_hash = ?", (content_hash,),
        ).fetchone()
    finally:
        connection.close()
    if row is None:
        return None
    keys = ("content_hash", "project", "status", "exit_code", "reason", "payload", "updated_at")
    return dict(zip(keys, row))


def aggregate(db_path: str | Path) -> dict[str, Any]:
    initialize(db_path)
    connection = _connect(db_path)
    try:
        rows = connection.execute("SELECT status, COUNT(*) FROM runs GROUP BY status").fetchall()
        reasons = connection.execute(
            "SELECT reason, COUNT(*) FROM runs WHERE reason IS NOT NULL GROUP BY reason ORDER BY reason"
        ).fetchall()
        total = connection.execute("SELECT COUNT(*) FROM runs").fetchone()[0]
    finally:
        connection.close()
    return {
        "total": int(total),
        "by_status": {str(status): int(count) for status, count in rows},
        "quarantine_reasons": {str(reason): int(count) for reason, count in reasons},
    }
