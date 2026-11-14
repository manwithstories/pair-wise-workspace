"""sqlite3 index: runs / packages / tracks / findings, one transaction per run."""

import datetime
import os
import sqlite3

SCHEMA = """
CREATE TABLE IF NOT EXISTS runs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  started_at TEXT NOT NULL,
  command TEXT NOT NULL,
  target TEXT,
  exit_code INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS packages(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sha256 TEXT NOT NULL UNIQUE,
  path TEXT NOT NULL,
  built_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tracks(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER NOT NULL REFERENCES runs(id),
  package_id INTEGER REFERENCES packages(id),
  file_path TEXT NOT NULL,
  container TEXT,
  track_index INTEGER,
  kind TEXT,
  codec TEXT,
  duration_us INTEGER,
  start_us INTEGER,
  summary TEXT);
CREATE TABLE IF NOT EXISTS findings(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER NOT NULL REFERENCES runs(id),
  rule_id TEXT NOT NULL,
  severity TEXT NOT NULL,
  expected TEXT,
  actual TEXT,
  track_index INTEGER,
  file_path TEXT);
CREATE INDEX IF NOT EXISTS idx_findings_severity ON findings(severity);
CREATE INDEX IF NOT EXISTS idx_findings_rule ON findings(rule_id);
CREATE INDEX IF NOT EXISTS idx_runs_started ON runs(started_at);
CREATE INDEX IF NOT EXISTS idx_tracks_path ON tracks(file_path);
CREATE INDEX IF NOT EXISTS idx_tracks_container ON tracks(container);
"""


def _now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(
        timespec="seconds")


class Store:
    def __init__(self, path):
        directory = os.path.dirname(os.path.abspath(path))
        os.makedirs(directory, exist_ok=True)
        self.conn = sqlite3.connect(path)
        self.conn.executescript(SCHEMA)

    def close(self):
        self.conn.close()

    def record_run(self, command, target, exit_code, file_results=(),
                   findings=(), package=None):
        """Persist one run (tracks, findings, optional package) atomically."""
        with self.conn:
            cur = self.conn.execute(
                "INSERT INTO runs(started_at, command, target, exit_code)"
                " VALUES(?,?,?,?)", (_now(), command, target, exit_code))
            run_id = cur.lastrowid
            package_id = None
            if package is not None:
                self.conn.execute("DELETE FROM packages WHERE sha256=?",
                                  (package["sha256"],))
                cur = self.conn.execute(
                    "INSERT INTO packages(sha256, path, built_at)"
                    " VALUES(?,?,?)",
                    (package["sha256"], package["path"], _now()))
                package_id = cur.lastrowid
            for result in file_results:
                for t in result.tracks:
                    self.conn.execute(
                        "INSERT INTO tracks(run_id, package_id, file_path,"
                        " container, track_index, kind, codec, duration_us,"
                        " start_us, summary) VALUES(?,?,?,?,?,?,?,?,?,?)",
                        (run_id, package_id, t.path, t.container,
                         t.track_index, t.kind, t.codec, t.duration_us,
                         t.start_us, t.summary()))
            for f in findings:
                self.conn.execute(
                    "INSERT INTO findings(run_id, rule_id, severity, expected,"
                    " actual, track_index, file_path) VALUES(?,?,?,?,?,?,?)",
                    (run_id, f.rule_id, f.severity, f.expected, f.actual,
                     f.track_index, f.file_path))
        return run_id

    def query(self, severity=None, rule_id=None, container=None, since=None,
              until=None, limit=20):
        sql = ("SELECT f.id, r.started_at, f.severity, f.rule_id,"
               " f.file_path, f.track_index, f.expected, f.actual"
               " FROM findings f JOIN runs r ON r.id = f.run_id WHERE 1=1")
        params = []
        if severity:
            sql += " AND f.severity = ?"
            params.append(severity)
        if rule_id:
            sql += " AND f.rule_id = ?"
            params.append(rule_id)
        if container:
            sql += (" AND f.file_path IN (SELECT file_path FROM tracks"
                    " WHERE container = ?)")
            params.append(container)
        if since:
            sql += " AND r.started_at >= ?"
            params.append(since)
        if until:
            sql += " AND r.started_at <= ?"
            params.append(until)
        total = self.conn.execute(
            "SELECT COUNT(*) FROM (%s)" % sql, params).fetchone()[0]
        rows = self.conn.execute(
            sql + " ORDER BY f.id DESC LIMIT ?", params + [limit]).fetchall()
        return rows, total
