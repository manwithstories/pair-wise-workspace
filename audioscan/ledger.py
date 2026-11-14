"""SQLite ledger with WAL, idempotent enqueue and restart-safe jobs."""

from __future__ import annotations

import hashlib
import os
import sqlite3
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable, Iterable, Sequence, TypeVar


T = TypeVar("T")
WRITE_RETRIES = 5


class LedgerBusyError(RuntimeError):
    """Raised after five consecutive locked/busy write failures."""


@dataclass(frozen=True)
class EnqueueResult:
    scanned: int
    queued: int
    skipped: int


@dataclass(frozen=True)
class JobRow:
    job_id: int
    file_id: int
    path: str
    sha256: str


def _now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def connect(path: str | os.PathLike[str]) -> sqlite3.Connection:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(target, timeout=1.0)
    try:
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA synchronous=NORMAL")
        conn.execute("PRAGMA busy_timeout=1000")
        conn.execute("PRAGMA foreign_keys=ON")
        return conn
    except sqlite3.OperationalError as exc:
        conn.close()
        if "locked" in str(exc).lower() or "busy" in str(exc).lower():
            raise LedgerBusyError("database remained locked") from exc
        raise


def init_db(conn: sqlite3.Connection) -> None:
    schema = (
        """
        CREATE TABLE IF NOT EXISTS files (
            id INTEGER PRIMARY KEY,
            sha256 TEXT NOT NULL UNIQUE,
            path TEXT NOT NULL,
            sample_rate INTEGER,
            channels INTEGER,
            total_samples INTEGER,
            encoding TEXT,
            channel_mode TEXT,
            status TEXT NOT NULL DEFAULT 'queued',
            error TEXT,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS jobs (
            id INTEGER PRIMARY KEY,
            file_id INTEGER NOT NULL UNIQUE REFERENCES files(id) ON DELETE CASCADE,
            status TEXT NOT NULL DEFAULT 'pending',
            attempts INTEGER NOT NULL DEFAULT 0,
            error TEXT,
            updated_at TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS segments (
            id INTEGER PRIMARY KEY,
            file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
            segment_index INTEGER NOT NULL,
            start_sample INTEGER NOT NULL,
            end_sample INTEGER NOT NULL,
            start_ms REAL NOT NULL,
            end_ms REAL NOT NULL,
            fade_in_ms INTEGER NOT NULL,
            fade_out_ms INTEGER NOT NULL,
            UNIQUE(file_id, segment_index)
        );
        CREATE TABLE IF NOT EXISTS config (
            key TEXT PRIMARY KEY,
            value TEXT NOT NULL
        );
        """
    )
    write_retry(conn, lambda: conn.executescript(schema))


def write_retry(conn: sqlite3.Connection, operation: Callable[[], T]) -> T:
    """Run one write transaction, retrying locked/busy failures five times."""

    for attempt in range(WRITE_RETRIES):
        try:
            with conn:
                return operation()
        except sqlite3.OperationalError as exc:
            message = str(exc).lower()
            conn.rollback()
            retryable = "locked" in message or "busy" in message
            if not retryable or attempt == WRITE_RETRIES - 1:
                if retryable:
                    raise LedgerBusyError("database remained locked") from exc
                raise
            time.sleep(0.05 * (attempt + 1))
    raise LedgerBusyError("database remained locked")


def hash_file(path: str | os.PathLike[str], chunk_size: int = 1024 * 1024) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            chunk = handle.read(chunk_size)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def _scan_wavs(root: str | os.PathLike[str]) -> list[Path]:
    source = Path(root)
    if source.is_symlink():
        return []
    if source.is_file():
        return [source] if source.suffix.lower() == ".wav" else []
    found: list[Path] = []
    for path in sorted(source.iterdir(), key=lambda item: item.name.lower()):
        if path.is_symlink() or not path.is_file():
            continue
        if path.suffix.lower() == ".wav":
            found.append(path)
    return found


def enqueue(conn: sqlite3.Connection, root: str | os.PathLike[str]) -> EnqueueResult:
    paths = _scan_wavs(root)
    now = _now()
    state = {"queued": 0}

    def operation() -> None:
        for path in paths:
            digest = hash_file(path)
            existing = conn.execute("SELECT id FROM files WHERE sha256 = ?", (digest,)).fetchone()
            if existing is not None:
                continue
            cursor = conn.execute(
                "INSERT INTO files(sha256, path, status, created_at, updated_at) VALUES (?, ?, 'queued', ?, ?)",
                (digest, os.fspath(path), now, now),
            )
            file_id = int(cursor.lastrowid)
            conn.execute(
                "INSERT INTO jobs(file_id, status, attempts, updated_at) VALUES (?, 'pending', 0, ?)",
                (file_id, now),
            )
            state["queued"] += 1

    write_retry(conn, operation)
    return EnqueueResult(len(paths), state["queued"], len(paths) - state["queued"])


def reset_running_jobs(conn: sqlite3.Connection) -> int:
    now = _now()

    def operation() -> int:
        cursor = conn.execute(
            "UPDATE jobs SET status='pending', updated_at=? WHERE status='running'", (now,)
        )
        return int(cursor.rowcount)

    return write_retry(conn, operation)


def pending_jobs(conn: sqlite3.Connection) -> list[JobRow]:
    rows = conn.execute(
        """
        SELECT j.id, j.file_id, f.path, f.sha256
        FROM jobs j JOIN files f ON f.id = j.file_id
        WHERE j.status = 'pending' AND f.status != 'failed'
        ORDER BY j.id
        """
    ).fetchall()
    return [JobRow(int(row["id"]), int(row["file_id"]), str(row["path"]), str(row["sha256"])) for row in rows]


def mark_running(conn: sqlite3.Connection, job_id: int) -> None:
    now = _now()
    write_retry(
        conn,
        lambda: conn.execute(
            "UPDATE jobs SET status='running', attempts=attempts+1, updated_at=? WHERE id=?",
            (now, int(job_id)),
        ),
    )


def mark_failed(conn: sqlite3.Connection, job_id: int, error: str) -> None:
    now = _now()

    def operation() -> None:
        row = conn.execute("SELECT file_id FROM jobs WHERE id=?", (int(job_id),)).fetchone()
        if row is None:
            return
        conn.execute(
            "UPDATE jobs SET status='failed', error=?, updated_at=? WHERE id=?",
            (error[:500], now, int(job_id)),
        )
        conn.execute(
            "UPDATE files SET status='failed', error=?, updated_at=? WHERE id=?",
            (error[:500], now, int(row["file_id"])),
        )

    write_retry(conn, operation)


def complete_job(
    conn: sqlite3.Connection,
    job_id: int,
    sample_rate: int,
    channels: int,
    total_samples: int,
    encoding: str,
    channel_mode: str,
    segments: Sequence[tuple[int, int, int, int]],
) -> None:
    now = _now()
    rows = []
    for index, (start, end, fade_in, fade_out) in enumerate(segments):
        rows.append(
            (
                int(start),
                int(end),
                start * 1000.0 / sample_rate,
                end * 1000.0 / sample_rate,
                int(fade_in),
                int(fade_out),
                index,
            )
        )

    def operation() -> None:
        row = conn.execute("SELECT file_id FROM jobs WHERE id=?", (int(job_id),)).fetchone()
        if row is None:
            raise ValueError("unknown job")
        file_id = int(row["file_id"])
        conn.execute(
            """
            UPDATE files SET sample_rate=?, channels=?, total_samples=?, encoding=?, channel_mode=?,
                status='done', error=NULL, updated_at=? WHERE id=?
            """,
            (sample_rate, channels, total_samples, encoding, channel_mode, now, file_id),
        )
        conn.execute("DELETE FROM segments WHERE file_id=?", (file_id,))
        for start, end, start_ms, end_ms, fade_in, fade_out, index in rows:
            conn.execute(
                """
                INSERT INTO segments(file_id, segment_index, start_sample, end_sample,
                    start_ms, end_ms, fade_in_ms, fade_out_ms)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (file_id, index, start, end, start_ms, end_ms, fade_in, fade_out),
            )
        conn.execute(
            "UPDATE jobs SET status='done', error=NULL, updated_at=? WHERE id=?",
            (now, int(job_id)),
        )

    write_retry(conn, operation)


def set_config(conn: sqlite3.Connection, key: str, value: str) -> None:
    write_retry(
        conn,
        lambda: conn.execute(
            "INSERT INTO config(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, value),
        ),
    )


def get_config(conn: sqlite3.Connection, key: str, default: str | None = None) -> str | None:
    row = conn.execute("SELECT value FROM config WHERE key=?", (key,)).fetchone()
    return default if row is None else str(row["value"])


def file_segments(conn: sqlite3.Connection, file_id: int) -> list[sqlite3.Row]:
    return conn.execute(
        "SELECT * FROM segments WHERE file_id=? ORDER BY segment_index", (int(file_id),)
    ).fetchall()


def iter_files(conn: sqlite3.Connection) -> Iterable[sqlite3.Row]:
    return conn.execute(
        """
        SELECT f.*, j.status AS job_status FROM files f
        LEFT JOIN jobs j ON j.file_id=f.id ORDER BY f.id
        """
    )
