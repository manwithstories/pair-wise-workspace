"""SQLite persistence with idempotent writes and one dedicated writer thread."""

from __future__ import annotations

import hashlib
import json
import queue
import sqlite3
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .model import SEVERITIES, SEVERITY_RANK, ProbeResult

SCHEMA = """
CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    sha256 TEXT NOT NULL,
    size INTEGER NOT NULL,
    container TEXT NOT NULL,
    status TEXT NOT NULL,
    error TEXT,
    scanned_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tracks (
    id INTEGER PRIMARY KEY,
    file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    track_index INTEGER NOT NULL,
    track_id INTEGER NOT NULL,
    kind TEXT NOT NULL,
    codec TEXT NOT NULL,
    timescale INTEGER NOT NULL,
    duration_ms REAL NOT NULL,
    presentation_duration_ms REAL NOT NULL,
    sample_count INTEGER NOT NULL,
    width INTEGER,
    height INTEGER,
    sample_rate INTEGER,
    channels INTEGER,
    sample_entry TEXT,
    codec_json TEXT NOT NULL,
    table_json TEXT NOT NULL,
    UNIQUE(file_id, track_index)
);
CREATE TABLE IF NOT EXISTS findings (
    id INTEGER PRIMARY KEY,
    file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    rule_id TEXT NOT NULL,
    severity TEXT NOT NULL,
    track_index INTEGER,
    offset INTEGER,
    evidence TEXT NOT NULL,
    message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_findings_rule ON findings(rule_id);
CREATE INDEX IF NOT EXISTS idx_findings_severity ON findings(severity);
CREATE INDEX IF NOT EXISTS idx_tracks_file ON tracks(file_id);
"""


class StoreError(RuntimeError):
    pass


def _fingerprint(result: ProbeResult) -> str:
    material = {
        "container": result.container,
        "size": result.file_size,
        "tracks": [track.public_dict() for track in result.tracks],
        "mdat_ranges": result.mdat_ranges,
        "movie_timescale": result.movie_timescale,
    }
    encoded = json.dumps(material, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


class Database:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.initialize()

    def connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(self.path, timeout=30)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys=ON")
        connection.execute("PRAGMA journal_mode=WAL")
        return connection

    def initialize(self) -> None:
        with self.connect() as connection:
            connection.executescript(SCHEMA)

    def writer(self) -> "WriterThread":
        return WriterThread(self.path)

    def summary(self) -> dict[str, Any]:
        with self.connect() as connection:
            files = connection.execute("SELECT COUNT(*) FROM files").fetchone()[0]
            tracks = connection.execute("SELECT COUNT(*) FROM tracks").fetchone()[0]
            findings = connection.execute("SELECT COUNT(*) FROM findings").fetchone()[0]
            severity_rows = connection.execute(
                "SELECT severity, COUNT(*) AS n FROM findings GROUP BY severity"
            ).fetchall()
        counts = {severity: 0 for severity in SEVERITIES}
        for row in severity_rows:
            counts[row["severity"]] = row["n"]
        return {"files": files, "tracks_total": tracks, "findings_total": findings,
                "findings_by_severity": counts}

    def findings(self, min_severity: str = "info") -> dict[str, Any]:
        if min_severity not in SEVERITY_RANK:
            raise StoreError(f"invalid severity: {min_severity}")
        allowed = [name for name in SEVERITIES if SEVERITY_RANK[name] <= SEVERITY_RANK[min_severity]]
        placeholders = ",".join("?" for _ in allowed)
        query = f"""
            SELECT f.rule_id, f.severity, f.track_index, f.offset, f.evidence, f.message,
                   files.path
            FROM findings f JOIN files ON files.id = f.file_id
            WHERE f.severity IN ({placeholders})
            ORDER BY files.path, f.rule_id, f.track_index, f.offset, f.id
        """
        with self.connect() as connection:
            rows = connection.execute(query, allowed).fetchall()
            rule_rows = connection.execute(
                f"""SELECT f.rule_id, COUNT(*) AS n FROM findings f
                    WHERE f.severity IN ({placeholders})
                    GROUP BY f.rule_id ORDER BY f.rule_id""",
                allowed,
            ).fetchall()
            severity_rows = connection.execute(
                f"""SELECT f.severity, COUNT(*) AS n FROM findings f
                    WHERE f.severity IN ({placeholders})
                    GROUP BY f.severity""",
                allowed,
            ).fetchall()
        by_severity = {severity: 0 for severity in SEVERITIES}
        for row in severity_rows:
            by_severity[row["severity"]] = row["n"]
        items = [{
            "path": row["path"], "rule_id": row["rule_id"], "severity": row["severity"],
            "track_index": row["track_index"], "offset": row["offset"],
            "evidence": row["evidence"], "message": row["message"],
        } for row in rows]
        return {
            "min_severity": min_severity,
            "total": len(items),
            "by_rule": {row["rule_id"]: row["n"] for row in rule_rows},
            "by_severity": by_severity,
            "findings": items,
        }


class WriterThread:
    def __init__(self, path: Path) -> None:
        self.path = path
        self._queue: queue.Queue = queue.Queue()
        self._thread = threading.Thread(target=self._run, name="mediaqc-writer", daemon=True)
        self._thread.start()

    def _run(self) -> None:
        connection = sqlite3.connect(self.path, timeout=30)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys=ON")
        connection.execute("PRAGMA journal_mode=WAL")
        connection.executescript(SCHEMA)
        while True:
            item = self._queue.get()
            if item is None:
                self._queue.task_done()
                break
            result, done, holder = item
            try:
                _write_result(connection, result)
                holder["value"] = True
            except Exception as exc:  # delivered to the submitting thread
                holder["error"] = exc
            finally:
                self._queue.task_done()
                done.set()
        connection.close()

    def write(self, result: ProbeResult) -> None:
        done = threading.Event()
        holder: dict[str, Any] = {}
        self._queue.put((result, done, holder))
        if not done.wait(timeout=60):
            raise StoreError("database writer timed out")
        if "error" in holder:
            raise StoreError(str(holder["error"])) from holder["error"]

    def close(self) -> None:
        if self._thread.is_alive():
            self._queue.put(None)
            self._thread.join(timeout=30)

    def __enter__(self) -> "WriterThread":
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        self.close()


def _write_result(connection: sqlite3.Connection, result: ProbeResult) -> None:
    canonical = str(Path(result.path).resolve())
    scanned_at = datetime.now(timezone.utc).isoformat(timespec="seconds")
    fingerprint = _fingerprint(result)
    with connection:
        connection.execute(
            """INSERT INTO files(path, sha256, size, container, status, error, scanned_at)
               VALUES(?, ?, ?, ?, ?, ?, ?)
               ON CONFLICT(path) DO UPDATE SET sha256=excluded.sha256, size=excluded.size,
               container=excluded.container, status=excluded.status,
               error=excluded.error, scanned_at=excluded.scanned_at""",
            (canonical, fingerprint, result.file_size, result.container, result.status,
             result.error, scanned_at),
        )
        file_id = connection.execute("SELECT id FROM files WHERE path=?", (canonical,)).fetchone()[0]
        connection.execute("DELETE FROM tracks WHERE file_id=?", (file_id,))
        connection.execute("DELETE FROM findings WHERE file_id=?", (file_id,))
        for track in result.tracks:
            connection.execute(
                """INSERT INTO tracks(file_id, track_index, track_id, kind, codec, timescale,
                   duration_ms, presentation_duration_ms, sample_count, width, height,
                   sample_rate, channels, sample_entry, codec_json, table_json)
                   VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (file_id, track.index, track.track_id, track.kind, track.codec, track.timescale,
                 track.duration_ms, track.presentation_duration_ms, track.sample_count,
                 track.width, track.height, track.sample_rate, track.channels,
                 track.sample_entry, json.dumps(track.codec_details, sort_keys=True),
                 json.dumps(track.table_facts, sort_keys=True)),
            )
        for finding in result.findings:
            connection.execute(
                """INSERT INTO findings(file_id, rule_id, severity, track_index, offset,
                   evidence, message) VALUES(?, ?, ?, ?, ?, ?, ?)""",
                (file_id, finding.rule_id, finding.severity, finding.track_index,
                 finding.offset, finding.evidence, finding.message),
            )
