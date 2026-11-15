"""SQLite schema, idempotent upserts, batches, and aggregate reporting."""

from __future__ import annotations

from datetime import datetime, timezone
import json
import os
from pathlib import Path
import sqlite3
from urllib.parse import quote

from .errors import DatabaseError


SCHEMA_VERSION = 1

SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS batches (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    params_fingerprint TEXT NOT NULL,
    params_json TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at TEXT NOT NULL,
    completed_at TEXT
);
CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    content_sha256 TEXT NOT NULL,
    param_fingerprint TEXT NOT NULL,
    source_path TEXT NOT NULL,
    sample_rate INTEGER NOT NULL,
    total_samples INTEGER NOT NULL,
    noise_floor_dbfs REAL NOT NULL,
    threshold_dbfs REAL NOT NULL,
    segment_count INTEGER NOT NULL,
    silence_total_samples INTEGER NOT NULL,
    silence_ratio REAL NOT NULL,
    dropped_segments INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(content_sha256, param_fingerprint)
);
CREATE TABLE IF NOT EXISTS segments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample INTEGER NOT NULL,
    duration_ms REAL NOT NULL,
    peak_dbfs REAL NOT NULL,
    UNIQUE(file_id, idx)
);
CREATE TABLE IF NOT EXISTS silences (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample INTEGER NOT NULL,
    duration_ms REAL NOT NULL,
    UNIQUE(file_id, idx)
);
CREATE TABLE IF NOT EXISTS batch_files (
    batch_id INTEGER NOT NULL REFERENCES batches(id) ON DELETE CASCADE,
    file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    PRIMARY KEY(batch_id, file_id)
);
CREATE TABLE IF NOT EXISTS exported_segments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    path TEXT NOT NULL,
    exported_at TEXT NOT NULL,
    UNIQUE(file_id, idx, path)
);
CREATE INDEX IF NOT EXISTS idx_segments_file ON segments(file_id);
CREATE INDEX IF NOT EXISTS idx_silences_file ON silences(file_id);
"""


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def parameter_fingerprint(params: dict) -> str:
    import hashlib

    encoded = json.dumps(params, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


class Store:
    def __init__(self, path: str | os.PathLike[str], *, readonly: bool = False):
        self.path = Path(path)
        self.readonly = readonly
        if readonly:
            if not self.path.exists():
                raise DatabaseError("database_unavailable", "database does not exist", path=str(self.path))
            uri = f"file:{quote(str(self.path.resolve()))}?mode=ro"
            target = uri
        else:
            try:
                self.path.parent.mkdir(parents=True, exist_ok=True)
            except OSError as exc:
                raise DatabaseError("database_unavailable", str(exc), path=str(self.path)) from exc
            target = str(self.path)
        try:
            self.conn = sqlite3.connect(target, uri=readonly, timeout=30.0)
            self.conn.row_factory = sqlite3.Row
            self.conn.execute("PRAGMA foreign_keys = ON")
            if not readonly:
                self.conn.execute("PRAGMA journal_mode = WAL")
            self.schema_version = self._migrate()
        except sqlite3.Error as exc:
            raise DatabaseError("database_error", str(exc), path=str(self.path)) from exc

    def __enter__(self) -> "Store":
        return self

    def __exit__(self, *exc_info) -> None:
        self.close()

    def close(self) -> None:
        if getattr(self, "conn", None) is not None:
            self.conn.close()
            self.conn = None

    def _migrate(self) -> int:
        version = int(self.conn.execute("PRAGMA user_version").fetchone()[0])
        if version > SCHEMA_VERSION:
            raise DatabaseError(
                "schema_too_new",
                f"database schema {version} is newer than supported {SCHEMA_VERSION}",
                path=str(self.path),
            )
        if version == 0:
            if not self.readonly:
                self.conn.executescript(SCHEMA)
                self.conn.execute(
                    "INSERT OR REPLACE INTO meta(key, value) VALUES('schema_version', ?)",
                    (str(SCHEMA_VERSION),),
                )
                self.conn.execute(f"PRAGMA user_version = {SCHEMA_VERSION}")
                self.conn.commit()
            else:
                tables = self.conn.execute(
                    "SELECT name FROM sqlite_master WHERE type='table'"
                ).fetchall()
                if tables:
                    raise DatabaseError(
                        "schema_migration_failed",
                        "read-only database has no schema version",
                        path=str(self.path),
                    )
        return max(version, SCHEMA_VERSION if not self.readonly else 0)

    def start_batch(self, kind: str, params: dict) -> int:
        fingerprint = parameter_fingerprint(params)
        try:
            cursor = self.conn.execute(
                "INSERT INTO batches(kind, params_fingerprint, params_json, status, started_at) "
                "VALUES(?, ?, ?, 'running', ?)",
                (kind, fingerprint, json.dumps(params, sort_keys=True), _now()),
            )
            self.conn.commit()
            return int(cursor.lastrowid)
        except sqlite3.Error as exc:
            raise DatabaseError("database_error", str(exc), path=str(self.path)) from exc

    def finish_batch(self, batch_id: int, status: str = "complete") -> None:
        try:
            self.conn.execute(
                "UPDATE batches SET status = ?, completed_at = ? WHERE id = ?",
                (status, _now(), batch_id),
            )
            self.conn.commit()
        except sqlite3.Error as exc:
            raise DatabaseError("database_error", str(exc), path=str(self.path)) from exc

    def record_scan(
        self,
        *,
        batch_id: int,
        source_path: str,
        content_sha256: str,
        params: dict,
        detection,
    ) -> int:
        fingerprint = parameter_fingerprint(params)
        total = detection.analysis.total_samples
        silence_samples = sum(item.end_sample - item.start_sample for item in detection.silences)
        silence_ratio = silence_samples / total if total else 0.0
        now = _now()
        try:
            with self.conn:
                row = self.conn.execute(
                    "SELECT id FROM files WHERE content_sha256 = ? AND param_fingerprint = ?",
                    (content_sha256, fingerprint),
                ).fetchone()
                values = (
                    source_path,
                    detection.analysis.sample_rate,
                    total,
                    detection.thresholds.noise_floor_dbfs,
                    detection.thresholds.enter_dbfs,
                    len(detection.segments),
                    silence_samples,
                    silence_ratio,
                    detection.dropped_segments,
                    now,
                )
                if row is None:
                    cursor = self.conn.execute(
                        "INSERT INTO files(source_path, sample_rate, total_samples, noise_floor_dbfs, "
                        "threshold_dbfs, segment_count, silence_total_samples, silence_ratio, "
                        "dropped_segments, created_at, updated_at, content_sha256, param_fingerprint) "
                        "VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                        values + (now, content_sha256, fingerprint),
                    )
                    file_id = int(cursor.lastrowid)
                else:
                    file_id = int(row["id"])
                    self.conn.execute(
                        "UPDATE files SET source_path=?, sample_rate=?, total_samples=?, "
                        "noise_floor_dbfs=?, threshold_dbfs=?, segment_count=?, "
                        "silence_total_samples=?, silence_ratio=?, dropped_segments=?, updated_at=? "
                        "WHERE id=?",
                        values + (file_id,),
                    )
                    self.conn.execute("DELETE FROM segments WHERE file_id = ?", (file_id,))
                    self.conn.execute("DELETE FROM silences WHERE file_id = ?", (file_id,))
                self._insert_children(file_id, detection)
                self.conn.execute(
                    "INSERT OR IGNORE INTO batch_files(batch_id, file_id) VALUES(?, ?)",
                    (batch_id, file_id),
                )
            return file_id
        except sqlite3.Error as exc:
            raise DatabaseError("database_error", str(exc), path=str(self.path)) from exc

    def _insert_children(self, file_id: int, detection) -> None:
        sample_rate = detection.analysis.sample_rate
        for index, segment in enumerate(detection.segments):
            self.conn.execute(
                "INSERT INTO segments(file_id, idx, start_sample, end_sample, duration_ms, peak_dbfs) "
                "VALUES(?, ?, ?, ?, ?, ?)",
                (
                    file_id,
                    index,
                    segment.start_sample,
                    segment.end_sample,
                    (segment.end_sample - segment.start_sample) * 1000.0 / sample_rate,
                    segment.peak_dbfs,
                ),
            )
        for index, silence in enumerate(detection.silences):
            self.conn.execute(
                "INSERT INTO silences(file_id, idx, start_sample, end_sample, duration_ms) "
                "VALUES(?, ?, ?, ?, ?)",
                (
                    file_id,
                    index,
                    silence.start_sample,
                    silence.end_sample,
                    (silence.end_sample - silence.start_sample) * 1000.0 / sample_rate,
                ),
            )

    def register_export(self, file_id: int, index: int, path: str) -> None:
        try:
            with self.conn:
                self.conn.execute(
                    "INSERT INTO exported_segments(file_id, idx, path, exported_at) VALUES(?, ?, ?, ?) "
                    "ON CONFLICT(file_id, idx, path) DO UPDATE SET exported_at=excluded.exported_at",
                    (file_id, index, path, _now()),
                )
        except sqlite3.Error as exc:
            raise DatabaseError("database_error", str(exc), path=str(self.path)) from exc

    def report(self) -> dict:
        try:
            files = self.conn.execute(
                "SELECT sample_rate, total_samples, segment_count, silence_total_samples, dropped_segments FROM files"
            ).fetchall()
            segments = self.conn.execute(
                "SELECT f.sample_rate, s.end_sample - s.start_sample AS samples "
                "FROM segments s JOIN files f ON f.id = s.file_id"
            ).fetchall()
            silences = self.conn.execute(
                "SELECT f.sample_rate, s.end_sample - s.start_sample AS samples "
                "FROM silences s JOIN files f ON f.id = s.file_id"
            ).fetchall()
        except sqlite3.Error as exc:
            raise DatabaseError("database_error", str(exc), path=str(self.path)) from exc
        duration_seconds = sum(row["total_samples"] / row["sample_rate"] for row in files)
        silence_seconds = sum(row["samples"] / row["sample_rate"] for row in silences)
        segment_seconds = sum(row["samples"] / row["sample_rate"] for row in segments)
        segment_count = sum(row["segment_count"] for row in files)
        longest = max((row["samples"] / row["sample_rate"] for row in silences), default=0.0)
        return {
            "file_count": len(files),
            "total_segments": segment_count,
            "total_silence_ms": round(silence_seconds * 1000.0, 3),
            "longest_silence_ms": round(longest * 1000.0, 3),
            "avg_segment_ms": round(segment_seconds * 1000.0 / segment_count, 3)
            if segment_count
            else 0.0,
            "silence_ratio": round(silence_seconds / duration_seconds, 6) if duration_seconds else 0.0,
            "dropped_segments": sum(row["dropped_segments"] for row in files),
            "schema_version": self.schema_version,
        }

