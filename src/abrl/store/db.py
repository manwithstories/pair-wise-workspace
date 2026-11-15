"""SQLite persistence: schema, WAL mode, atomic claims and resume queries."""
from __future__ import annotations

import sqlite3
from contextlib import contextmanager
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS tasks (
    key TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    width INTEGER NOT NULL,
    height INTEGER NOT NULL,
    fps INTEGER NOT NULL,
    qp INTEGER NOT NULL,
    gop INTEGER NOT NULL,
    encoder_version TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    bytes INTEGER,
    duration_s REAL,
    actual_kbps REAL,
    psnr_db REAL,
    psnr_min_db REAL,
    ssim REAL,
    ssim_min REAL,
    bitstream_path TEXT,
    error TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
"""

TERMINAL = {"done", "failed"}


class DatabaseError(RuntimeError):
    """Raised on schema or input problems inside the local store."""


def connect(db_path: str | Path) -> sqlite3.Connection:
    path = Path(db_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path), timeout=30, isolation_level=None)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")
    conn.execute("PRAGMA busy_timeout=30000")
    conn.execute("PRAGMA foreign_keys=ON")
    return conn


def initialize(conn: sqlite3.Connection) -> None:
    conn.executescript(SCHEMA)


@contextmanager
def transaction(conn: sqlite3.Connection):
    conn.execute("BEGIN IMMEDIATE")
    try:
        yield
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise


def validate_schema(conn: sqlite3.Connection) -> None:
    cols = {row[1] for row in conn.execute("PRAGMA table_info(tasks)")}
    required = {"key", "status", "actual_kbps", "psnr_db", "ssim", "bytes", "duration_s", "encoder_version"}
    missing = required - cols
    if missing:
        raise DatabaseError(f"tasks table missing columns: {', '.join(sorted(missing))}")


def upsert_pending(conn: sqlite3.Connection, specs: list) -> int:
    inserted = 0
    with transaction(conn):
        for spec in specs:
            cur = conn.execute("SELECT 1 FROM tasks WHERE key=?", (spec.key,))
            if cur.fetchone() is None:
                conn.execute(
                    """INSERT INTO tasks (key,source,source_digest,width,height,fps,qp,gop,encoder_version)
                       VALUES (?,?,?,?,?,?,?,?,?)""",
                    (spec.key, spec.source, spec.source_digest, spec.width, spec.height,
                     spec.fps, spec.qp, spec.gop, spec.encoder_version),
                )
                inserted += 1
    return inserted


def existing_params(conn: sqlite3.Connection) -> dict[str, dict]:
    return {row["key"]: dict(row) for row in conn.execute("SELECT * FROM tasks")}


def reclaim_orphans(conn: sqlite3.Connection) -> int:
    """Mark stale 'running' rows as orphaned and requeue them as pending."""
    with transaction(conn):
        cur = conn.execute(
            "UPDATE tasks SET status='pending', updated_at=datetime('now') "
            "WHERE status='running'"
        )
        return cur.rowcount


def counts(conn: sqlite3.Connection) -> dict[str, int]:
    rows = conn.execute("SELECT status, COUNT(*) AS n FROM tasks GROUP BY status")
    stats = {row["status"]: row["n"] for row in rows}
    stats["total"] = sum(stats.values())
    return stats


def claim_next(conn: sqlite3.Connection) -> dict | None:
    """Atomically claim one pending task; returns its row dict or None."""
    with transaction(conn):
        row = conn.execute(
            "SELECT * FROM tasks WHERE status='pending' ORDER BY rowid LIMIT 1"
        ).fetchone()
        if row is None:
            return None
        conn.execute(
            "UPDATE tasks SET status='running', attempts=attempts+1, updated_at=datetime('now') "
            "WHERE key=?",
            (row["key"],),
        )
    return dict(row)


def mark_done(conn: sqlite3.Connection, key: str, result: dict) -> None:
    with transaction(conn):
        conn.execute(
            """UPDATE tasks SET status='done', bytes=:bytes, duration_s=:duration_s,
                   actual_kbps=:actual_kbps, psnr_db=:psnr_db, psnr_min_db=:psnr_min_db,
                   ssim=:ssim, ssim_min=:ssim_min, bitstream_path=:bitstream_path,
                   error=NULL, updated_at=datetime('now') WHERE key=:key""",
            {"key": key, **result},
        )


def mark_failed(conn: sqlite3.Connection, key: str, message: str) -> None:
    with transaction(conn):
        conn.execute(
            "UPDATE tasks SET status='failed', error=?, updated_at=datetime('now') WHERE key=?",
            (message, key),
        )


def requeue_failed_attempt(conn: sqlite3.Connection, key: str, message: str) -> None:
    with transaction(conn):
        conn.execute(
            "UPDATE tasks SET status='pending', error=?, updated_at=datetime('now') WHERE key=?",
            (message, key),
        )


def completed_rows(conn: sqlite3.Connection) -> list[dict]:
    return [dict(row) for row in conn.execute(
        "SELECT * FROM tasks WHERE status='done' ORDER BY actual_kbps"
    )]


def get(conn: sqlite3.Connection, key: str) -> dict | None:
    row = conn.execute("SELECT * FROM tasks WHERE key=?", (key,)).fetchone()
    return dict(row) if row else None
