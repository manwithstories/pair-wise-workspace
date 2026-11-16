"""SQLite persistence for batches, file digests, segments and silences.

Files are keyed by SHA-256 so re-processing identical content is idempotent:
the old rows are replaced inside a single transaction rather than appended.
All writes for one batch happen in one transaction; a failure rolls the whole
batch back, so a broken file can never leave a half-written result set.
"""

from __future__ import annotations

import json
import os
import sqlite3
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Any, Iterator, Optional, Sequence

from .model import DetectionConfig, DetectionResult, FileAnalysis

SCHEMA_VERSION = 1

SCHEMA = """
CREATE TABLE IF NOT EXISTS schema_info (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS batches (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at  TEXT    NOT NULL,
    root        TEXT    NOT NULL,
    config_json TEXT    NOT NULL,
    status      TEXT    NOT NULL
);
CREATE TABLE IF NOT EXISTS files (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    sha256              TEXT    NOT NULL UNIQUE,
    path                TEXT    NOT NULL,
    status              TEXT    NOT NULL,
    sample_rate         INTEGER,
    channels            INTEGER,
    sample_width        INTEGER,
    total_samples       INTEGER,
    duration_seconds    REAL,
    noise_floor_db      REAL,
    enter_threshold_db  REAL,
    exit_threshold_db   REAL,
    error_code          TEXT,
    error_message       TEXT,
    updated_at          TEXT    NOT NULL
);
CREATE TABLE IF NOT EXISTS segments (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id      INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    idx          INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample   INTEGER NOT NULL,
    sample_count INTEGER NOT NULL,
    peak         REAL    NOT NULL,
    rms          REAL    NOT NULL,
    UNIQUE(file_id, idx)
);
CREATE TABLE IF NOT EXISTS silences (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id      INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    idx          INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample   INTEGER NOT NULL,
    sample_count INTEGER NOT NULL,
    UNIQUE(file_id, idx)
);
CREATE TABLE IF NOT EXISTS batch_files (
    batch_id INTEGER NOT NULL REFERENCES batches(id) ON DELETE CASCADE,
    file_id  INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    PRIMARY KEY (batch_id, file_id)
);
CREATE INDEX IF NOT EXISTS idx_segments_file ON segments(file_id);
CREATE INDEX IF NOT EXISTS idx_silences_file ON silences(file_id);
CREATE INDEX IF NOT EXISTS idx_files_sha ON files(sha256);
"""


class StoreError(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def _utc_now() -> str:
    from datetime import datetime, timezone

    return datetime.now(timezone.utc).isoformat(timespec="seconds")


class Store:
    """Thin sqlite3 wrapper; one instance per database path."""

    def __init__(self, path: str) -> None:
        self.path = path
        parent = os.path.dirname(os.path.abspath(path))
        if parent and not os.path.isdir(parent):
            raise StoreError(
                "db_dir_missing", f"database directory does not exist: {parent}"
            )
        try:
            self._conn = sqlite3.connect(path)
            self._conn.row_factory = sqlite3.Row
            self._conn.execute("PRAGMA foreign_keys = ON")
            self._conn.execute("PRAGMA journal_mode = WAL")
            self._conn.executescript(SCHEMA)
            self._conn.execute(
                "INSERT OR IGNORE INTO schema_info(key, value) VALUES('version', ?)",
                (str(SCHEMA_VERSION),),
            )
            self._conn.commit()
        except sqlite3.Error as exc:
            raise StoreError("db_open_failed", str(exc)) from exc

    def close(self) -> None:
        self._conn.close()

    def __enter__(self) -> "Store":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    @contextmanager
    def transaction(self) -> Iterator[sqlite3.Connection]:
        try:
            yield self._conn
            self._conn.commit()
        except Exception:
            self._conn.rollback()
            raise

    # ---------------------------------------------------------------- batches

    def begin_batch(self, root: str, config: DetectionConfig) -> int:
        with self.transaction() as conn:
            cur = conn.execute(
                "INSERT INTO batches(created_at, root, config_json, status)"
                " VALUES(?, ?, ?, 'running')",
                (_utc_now(), root, json.dumps(config.to_dict(), sort_keys=True)),
            )
            return int(cur.lastrowid)

    def finish_batch(self, batch_id: int, status: str) -> None:
        with self.transaction() as conn:
            conn.execute("UPDATE batches SET status=? WHERE id=?", (status, batch_id))

    def batch_summary(self, batch_id: int) -> dict[str, Any]:
        row = self._conn.execute(
            "SELECT * FROM batches WHERE id=?", (batch_id,)
        ).fetchone()
        if row is None:
            raise StoreError("batch_not_found", f"no batch with id {batch_id}")
        return dict(row)

    # ------------------------------------------------------------------ files

    def save_analysis(self, analysis: FileAnalysis, batch_id: Optional[int]) -> int:
        """Insert or replace one file's digest + results; returns file_id."""

        with self.transaction() as conn:
            file_id = self._save_analysis(conn, analysis)
            if batch_id is not None:
                conn.execute(
                    "INSERT OR IGNORE INTO batch_files(batch_id, file_id) VALUES(?, ?)",
                    (batch_id, file_id),
                )
            return file_id

    def _save_analysis(self, conn: sqlite3.Connection, analysis: FileAnalysis) -> int:
        info = analysis.info
        result = analysis.result
        conn.execute(
            """
            INSERT INTO files(sha256, path, status, sample_rate, channels,
                              sample_width, total_samples, duration_seconds,
                              noise_floor_db, enter_threshold_db, exit_threshold_db,
                              error_code, error_message, updated_at)
            VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(sha256) DO UPDATE SET
                path=excluded.path,
                status=excluded.status,
                sample_rate=excluded.sample_rate,
                channels=excluded.channels,
                sample_width=excluded.sample_width,
                total_samples=excluded.total_samples,
                duration_seconds=excluded.duration_seconds,
                noise_floor_db=excluded.noise_floor_db,
                enter_threshold_db=excluded.enter_threshold_db,
                exit_threshold_db=excluded.exit_threshold_db,
                error_code=excluded.error_code,
                error_message=excluded.error_message,
                updated_at=excluded.updated_at
            """,
            (
                analysis.sha256,
                analysis.path,
                analysis.status,
                info.sample_rate if info else None,
                info.channels if info else None,
                info.sample_width if info else None,
                info.nframes if info else None,
                info.duration_seconds if info else None,
                result.noise_floor_db if result else None,
                result.enter_threshold_db if result else None,
                result.exit_threshold_db if result else None,
                analysis.error_code,
                analysis.error_message,
                _utc_now(),
            ),
        )
        row = conn.execute(
            "SELECT id FROM files WHERE sha256=?", (analysis.sha256,)
        ).fetchone()
        if row is None:  # pragma: no cover - defensive
            raise StoreError("db_write_failed", "file row disappeared after upsert")
        file_id = int(row["id"])
        conn.execute("DELETE FROM segments WHERE file_id=?", (file_id,))
        conn.execute("DELETE FROM silences WHERE file_id=?", (file_id,))
        if result is not None:
            conn.executemany(
                "INSERT INTO segments(file_id, idx, start_sample, end_sample,"
                " sample_count, peak, rms) VALUES(?, ?, ?, ?, ?, ?, ?)",
                [
                    (
                        file_id,
                        seg.index,
                        seg.start_sample,
                        seg.end_sample,
                        seg.sample_count,
                        seg.peak,
                        seg.rms,
                    )
                    for seg in result.segments
                ],
            )
            conn.executemany(
                "INSERT INTO silences(file_id, idx, start_sample, end_sample,"
                " sample_count) VALUES(?, ?, ?, ?, ?)",
                [
                    (file_id, sil.index, sil.start_sample, sil.end_sample,
                     sil.sample_count)
                    for sil in result.silences
                ],
            )
        return file_id

    def get_file_by_hash(self, sha256: str) -> Optional[dict[str, Any]]:
        row = self._conn.execute(
            "SELECT * FROM files WHERE sha256=?", (sha256,)
        ).fetchone()
        return dict(row) if row else None

    def list_segments(self, file_id: int) -> list[dict[str, Any]]:
        rows = self._conn.execute(
            "SELECT * FROM segments WHERE file_id=? ORDER BY idx", (file_id,)
        ).fetchall()
        return [dict(r) for r in rows]

    def list_silences(self, file_id: int) -> list[dict[str, Any]]:
        rows = self._conn.execute(
            "SELECT * FROM silences WHERE file_id=? ORDER BY idx", (file_id,)
        ).fetchall()
        return [dict(r) for r in rows]

    def batch_files(self, batch_id: int) -> list[dict[str, Any]]:
        rows = self._conn.execute(
            "SELECT f.* FROM files f JOIN batch_files bf ON bf.file_id=f.id"
            " WHERE bf.batch_id=? ORDER BY f.path",
            (batch_id,),
        ).fetchall()
        return [dict(r) for r in rows]

    def query_segments(self, batch_id: Optional[int] = None) -> list[dict[str, Any]]:
        """Reusable SQL view joining segments back to their file."""

        sql = (
            "SELECT f.path, f.sha256, s.idx, s.start_sample, s.end_sample,"
            " s.sample_count, s.peak, s.rms FROM segments s JOIN files f ON f.id=s.file_id"
        )
        params: Sequence[Any] = ()
        if batch_id is not None:
            sql += " JOIN batch_files bf ON bf.file_id=f.id AND bf.batch_id=?"
            params = (batch_id,)
        sql += " ORDER BY f.path, s.idx"
        rows = self._conn.execute(sql, params).fetchall()
        return [dict(r) for r in rows]

    def stats(self) -> dict[str, Any]:
        files = self._conn.execute("SELECT COUNT(*) FROM files").fetchone()[0]
        segs = self._conn.execute("SELECT COUNT(*) FROM segments").fetchone()[0]
        return {"files": int(files), "segments": int(segs)}
