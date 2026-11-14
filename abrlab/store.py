"""SQLite persistence, state transitions and crash recovery."""

from __future__ import annotations

import json
import sqlite3
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
    task_key TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    source_json TEXT NOT NULL,
    params_json TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','running','done','failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    elapsed_sec REAL NOT NULL DEFAULT 0,
    bytes INTEGER NOT NULL DEFAULT 0,
    error_code TEXT,
    error_message TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks(status);
CREATE TABLE IF NOT EXISTS results (
    task_key TEXT PRIMARY KEY REFERENCES tasks(task_key) ON DELETE CASCADE,
    payload BLOB NOT NULL,
    bitrate_kbps REAL NOT NULL,
    psnr_db REAL NOT NULL,
    ssim REAL NOT NULL,
    bytes INTEGER NOT NULL,
    elapsed_sec REAL NOT NULL,
    created_at TEXT NOT NULL
);
"""


class StoreError(RuntimeError):
    """Raised for database state or integrity failures."""


def _now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds")


class Store:
    def __init__(self, path: str | Path):
        self.path = str(path)
        if self.path != ":memory:":
            Path(self.path).parent.mkdir(parents=True, exist_ok=True)
        try:
            self.conn = sqlite3.connect(self.path, timeout=10.0)
            self.conn.row_factory = sqlite3.Row
            self.conn.execute("PRAGMA foreign_keys = ON")
            self.conn.execute("PRAGMA busy_timeout = 10000")
            if self.path != ":memory:":
                self.conn.execute("PRAGMA journal_mode = WAL")
                self.conn.execute("PRAGMA synchronous = NORMAL")
            with self.conn:
                self.conn.executescript(SCHEMA)
                self.conn.execute(
                    "INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)",
                    ("schema_version", "1"),
                )
        except sqlite3.Error as exc:
            raise StoreError(f"cannot initialize database: {exc}") from exc

    def close(self) -> None:
        self.conn.close()

    def __enter__(self) -> "Store":
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        self.close()

    def add_plan_entries(self, entries: list[dict[str, Any]]) -> int:
        inserted = 0
        now = _now()
        try:
            with self.conn:
                for entry in entries:
                    parameters = (
                        entry["task_key"],
                        entry["name"],
                        json.dumps(entry["source"], sort_keys=True, separators=(",", ":")),
                        json.dumps(entry["params"], sort_keys=True, separators=(",", ":")),
                        "pending",
                        now,
                        now,
                    )
                    cursor = self.conn.execute(
                        "INSERT OR IGNORE INTO tasks("
                        "task_key,name,source_json,params_json,status,created_at,updated_at"
                        ") VALUES (?,?,?,?,?,?,?)",
                        parameters,
                    )
                    inserted += cursor.rowcount
        except sqlite3.IntegrityError as exc:
            raise StoreError(f"plan conflicts with existing database: {exc}") from exc
        except sqlite3.Error as exc:
            raise StoreError(f"cannot persist plan: {exc}") from exc
        return inserted

    def recover_stale(self) -> int:
        try:
            with self.conn:
                cursor = self.conn.execute(
                    "UPDATE tasks SET status=?, error_code=?, error_message=?, updated_at=? "
                    "WHERE status=?",
                    ("pending", "E_RECOVERED", "stale running task recovered", _now(), "running"),
                )
            return cursor.rowcount
        except sqlite3.Error as exc:
            raise StoreError(f"cannot recover stale tasks: {exc}") from exc

    def resumable_tasks(self) -> list[sqlite3.Row]:
        try:
            return list(
                self.conn.execute(
                    "SELECT * FROM tasks WHERE status IN (?,?) ORDER BY created_at, task_key",
                    ("pending", "running"),
                )
            )
        except sqlite3.Error as exc:
            raise StoreError(f"cannot list resumable tasks: {exc}") from exc

    def claim_task(self, key: str) -> int | None:
        try:
            with self.conn:
                cursor = self.conn.execute(
                    "UPDATE tasks SET status=?, attempts=attempts+1, error_code=NULL, "
                    "error_message=NULL, updated_at=? WHERE task_key=? AND status=? AND attempts<?",
                    ("running", _now(), key, "pending", 3),
                )
                if cursor.rowcount != 1:
                    return None
                row = self.conn.execute(
                    "SELECT attempts FROM tasks WHERE task_key=?", (key,)
                ).fetchone()
            return int(row["attempts"])
        except sqlite3.Error as exc:
            raise StoreError(f"cannot claim task: {exc}") from exc

    def complete_task(
        self,
        key: str,
        payload: bytes,
        bitrate_kbps: float,
        psnr_db: float,
        ssim: float,
        elapsed_sec: float,
    ) -> None:
        now = _now()
        try:
            with self.conn:
                previous = self.conn.execute(
                    "SELECT elapsed_sec FROM tasks WHERE task_key=? AND status=?", (key, "running")
                ).fetchone()
                if previous is None:
                    raise StoreError("task was not in running state")
                total_elapsed = float(previous["elapsed_sec"]) + elapsed_sec
                self.conn.execute(
                    "INSERT INTO results(task_key,payload,bitrate_kbps,psnr_db,ssim,bytes,"
                    "elapsed_sec,created_at) VALUES (?,?,?,?,?,?,?,?)",
                    (key, sqlite3.Binary(payload), bitrate_kbps, psnr_db, ssim,
                     len(payload), total_elapsed, now),
                )
                cursor = self.conn.execute(
                    "UPDATE tasks SET status=?, elapsed_sec=?, bytes=?, error_code=NULL, "
                    "error_message=NULL, updated_at=? WHERE task_key=? AND status=?",
                    ("done", total_elapsed, len(payload), now, key, "running"),
                )
                if cursor.rowcount != 1:
                    raise StoreError("task was not in running state")
        except sqlite3.Error as exc:
            raise StoreError(f"cannot complete task: {exc}") from exc

    def fail_task(self, key: str, error_code: str, message: str, elapsed_sec: float) -> str:
        try:
            with self.conn:
                row = self.conn.execute(
                    "SELECT attempts, elapsed_sec FROM tasks WHERE task_key=?", (key,)
                ).fetchone()
                if row is None:
                    raise StoreError("cannot fail an unknown task")
                status = "failed" if int(row["attempts"]) >= 3 else "pending"
                self.conn.execute(
                    "UPDATE tasks SET status=?, elapsed_sec=?, error_code=?, error_message=?, "
                    "updated_at=? WHERE task_key=?",
                    (status, float(row["elapsed_sec"]) + elapsed_sec, error_code,
                     message[:1000], _now(), key),
                )
            return status
        except sqlite3.Error as exc:
            raise StoreError(f"cannot record task failure: {exc}") from exc

    def finalize_exhausted(self) -> int:
        try:
            with self.conn:
                cursor = self.conn.execute(
                    "UPDATE tasks SET status=?, error_code=?, error_message=?, updated_at=? "
                    "WHERE status=? AND attempts>=?",
                    ("failed", "E_RETRY_EXHAUSTED", "retry limit exhausted", _now(), "pending", 3),
                )
            return cursor.rowcount
        except sqlite3.Error as exc:
            raise StoreError(f"cannot finalize exhausted tasks: {exc}") from exc

    def status_counts(self) -> dict[str, int]:
        counts = {"pending": 0, "running": 0, "done": 0, "failed": 0}
        try:
            for row in self.conn.execute("SELECT status, COUNT(*) AS n FROM tasks GROUP BY status"):
                counts[str(row["status"])] = int(row["n"])
        except sqlite3.Error as exc:
            raise StoreError(f"cannot count tasks: {exc}") from exc
        return counts

    def result_rows(self) -> list[sqlite3.Row]:
        try:
            return list(
                self.conn.execute(
                    "SELECT t.task_key,t.name,t.source_json,t.params_json,t.status,"
                    "r.bitrate_kbps,r.psnr_db,r.ssim,r.bytes,r.elapsed_sec,r.payload "
                    "FROM tasks t JOIN results r ON r.task_key=t.task_key "
                    "WHERE t.status=? ORDER BY r.bitrate_kbps, t.task_key",
                    ("done",),
                )
            )
        except sqlite3.Error as exc:
            raise StoreError(f"cannot load results: {exc}") from exc

    def get_result(self, key: str) -> sqlite3.Row | None:
        try:
            return self.conn.execute(
                "SELECT t.task_key,t.name,t.source_json,t.params_json,t.status,"
                "r.bitrate_kbps,r.psnr_db,r.ssim,r.bytes,r.elapsed_sec,r.payload "
                "FROM tasks t LEFT JOIN results r ON r.task_key=t.task_key "
                "WHERE t.task_key=?",
                (key,),
            ).fetchone()
        except sqlite3.Error as exc:
            raise StoreError(f"cannot load task result: {exc}") from exc

    def task_attempts(self, key: str) -> int:
        row = self.conn.execute("SELECT attempts FROM tasks WHERE task_key=?", (key,)).fetchone()
        return int(row["attempts"]) if row else 0
