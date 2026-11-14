"""SQLite index: files / segments / silences / runs, idempotent by sha256."""

from __future__ import annotations

import sqlite3
import time

SCHEMA = """
CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    sha256 TEXT NOT NULL,
    sample_rate INTEGER NOT NULL,
    channels INTEGER NOT NULL,
    total_samples INTEGER NOT NULL,
    status TEXT NOT NULL,
    error TEXT,
    scanned_at REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS segments (
    file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample INTEGER NOT NULL,
    PRIMARY KEY (file_id, idx)
);
CREATE TABLE IF NOT EXISTS silences (
    file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample INTEGER NOT NULL,
    PRIMARY KEY (file_id, idx)
);
CREATE TABLE IF NOT EXISTS runs (
    id INTEGER PRIMARY KEY,
    started_at REAL NOT NULL,
    finished_at REAL,
    files_ok INTEGER NOT NULL DEFAULT 0,
    files_failed INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL
);
"""


class Store:
    def __init__(self, path):
        self.conn = sqlite3.connect(str(path))
        self.conn.execute("PRAGMA foreign_keys = ON")
        self.conn.executescript(SCHEMA)

    def close(self):
        self.conn.close()

    def known_digest(self, path: str):
        row = self.conn.execute(
            "SELECT sha256, status FROM files WHERE path = ?", (path,)).fetchone()
        return row[0] if row and row[1] == "ok" else None

    def record_failure(self, path, sha256, info, error):
        with self.conn:  # rolls back on exception
            self.conn.execute("DELETE FROM files WHERE path = ?", (path,))
            self.conn.execute(
                "INSERT INTO files (path, sha256, sample_rate, channels,"
                " total_samples, status, error, scanned_at)"
                " VALUES (?,?,?,?,?,?,?,?)",
                (path, sha256 or "", info.get("sample_rate", 0),
                 info.get("channels", 0), info.get("total_samples", 0),
                 "failed", str(error), time.time()))

    def record_result(self, path, sha256, info, segments, silences):
        """Single transaction: replace file row + ordered segment/silence rows."""
        with self.conn:
            cur = self.conn.execute(
                "INSERT INTO files (path, sha256, sample_rate, channels,"
                " total_samples, status, error, scanned_at)"
                " VALUES (?,?,?,?,?,?,?,?)"
                " ON CONFLICT(path) DO UPDATE SET sha256=excluded.sha256,"
                " sample_rate=excluded.sample_rate, channels=excluded.channels,"
                " total_samples=excluded.total_samples, status='ok', error=NULL,"
                " scanned_at=excluded.scanned_at RETURNING id",
                (path, sha256, info["sample_rate"], info["channels"],
                 info["total_samples"], "ok", None, time.time()))
            file_id = cur.fetchone()[0]
            self.conn.execute("DELETE FROM segments WHERE file_id = ?", (file_id,))
            self.conn.execute("DELETE FROM silences WHERE file_id = ?", (file_id,))
            self.conn.executemany(
                "INSERT INTO segments (file_id, idx, start_sample, end_sample)"
                " VALUES (?,?,?,?)",
                [(file_id, i, s.start_sample, s.end_sample)
                 for i, s in enumerate(segments)])
            self.conn.executemany(
                "INSERT INTO silences (file_id, idx, start_sample, end_sample)"
                " VALUES (?,?,?,?)",
                [(file_id, i, s.start_sample, s.end_sample)
                 for i, s in enumerate(silences)])

    def begin_run(self) -> int:
        with self.conn:
            cur = self.conn.execute(
                "INSERT INTO runs (started_at, status) VALUES (?, 'running')",
                (time.time(),))
            return cur.lastrowid

    def finish_run(self, run_id, ok, failed, status):
        with self.conn:
            self.conn.execute(
                "UPDATE runs SET finished_at=?, files_ok=?, files_failed=?,"
                " status=? WHERE id=?",
                (time.time(), ok, failed, status, run_id))

    def files(self, ok_only=True):
        sql = "SELECT id, path, sample_rate, total_samples, status, error FROM files"
        if ok_only:
            sql += " WHERE status = 'ok'"
        return self.conn.execute(sql + " ORDER BY path").fetchall()

    def intervals(self, table, file_id):
        assert table in ("segments", "silences")
        return self.conn.execute(
            f"SELECT start_sample, end_sample FROM {table}"
            " WHERE file_id = ? ORDER BY idx", (file_id,)).fetchall()
