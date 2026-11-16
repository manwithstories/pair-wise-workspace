"""SQLite persistence: runs / segments / silences, idempotent replace."""
from __future__ import annotations

import json
import sqlite3
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    sha256 TEXT NOT NULL,
    source_path TEXT NOT NULL,
    sample_rate INTEGER NOT NULL,
    total_samples INTEGER NOT NULL,
    params_json TEXT NOT NULL,
    noise_floor_dbfs REAL NOT NULL,
    threshold_dbfs REAL NOT NULL,
    UNIQUE (sha256, params_json)
);
CREATE TABLE IF NOT EXISTS segments (
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample INTEGER NOT NULL,
    fade_in_samples INTEGER NOT NULL,
    fade_out_samples INTEGER NOT NULL,
    PRIMARY KEY (run_id, ordinal)
);
CREATE TABLE IF NOT EXISTS silences (
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample INTEGER NOT NULL,
    duration_samples INTEGER NOT NULL,
    PRIMARY KEY (run_id, ordinal)
);
"""


class DatabaseError(Exception):
    """SQLite read/write failure; maps to exit code 4."""


def open_db(path):
    try:
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        conn = sqlite3.connect(str(path))
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA foreign_keys=ON")
        conn.executescript(SCHEMA)
    except sqlite3.Error as exc:
        raise DatabaseError(f"cannot open database {path}: {exc}") from exc
    return conn


def close_db(conn):
    try:
        db_path = conn.execute(
            "SELECT file FROM pragma_database_list WHERE name='main'"
        ).fetchone()[0]
        conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        conn.execute("PRAGMA journal_mode=DELETE")
        conn.close()
        for suffix in ("-wal", "-shm"):
            sidecar = Path(db_path + suffix)
            if sidecar.exists():
                sidecar.unlink()
    except sqlite3.Error as exc:
        raise DatabaseError(f"cannot close database: {exc}") from exc


def save_run(conn, *, sha256, source_path, sample_rate, total_samples, params,
             noise_floor_dbfs, threshold_dbfs, segments, silences):
    """Insert a run; same (sha256, params) replaces in one transaction."""
    params_json = json.dumps(params, sort_keys=True, separators=(",", ":"))
    try:
        with conn:
            row = conn.execute(
                "SELECT id FROM runs WHERE sha256=? AND params_json=?",
                (sha256, params_json)).fetchone()
            if row is None:
                cur = conn.execute(
                    "INSERT INTO runs (sha256, source_path, sample_rate,"
                    " total_samples, params_json, noise_floor_dbfs,"
                    " threshold_dbfs) VALUES (?,?,?,?,?,?,?)",
                    (sha256, source_path, sample_rate, total_samples,
                     params_json, noise_floor_dbfs, threshold_dbfs))
                run_id = cur.lastrowid
            else:
                run_id = row[0]
                conn.execute("DELETE FROM segments WHERE run_id=?", (run_id,))
                conn.execute("DELETE FROM silences WHERE run_id=?", (run_id,))
                conn.execute(
                    "UPDATE runs SET source_path=?, sample_rate=?,"
                    " total_samples=?, noise_floor_dbfs=?, threshold_dbfs=?"
                    " WHERE id=?",
                    (source_path, sample_rate, total_samples,
                     noise_floor_dbfs, threshold_dbfs, run_id))
            conn.executemany(
                "INSERT INTO segments (run_id, ordinal, start_sample,"
                " end_sample, fade_in_samples, fade_out_samples)"
                " VALUES (?,?,?,?,?,?)",
                [(run_id, s.ordinal, s.start_sample, s.end_sample,
                  s.fade_in_samples, s.fade_out_samples) for s in segments])
            conn.executemany(
                "INSERT INTO silences (run_id, ordinal, start_sample,"
                " end_sample, duration_samples) VALUES (?,?,?,?,?)",
                [(run_id, s.ordinal, s.start_sample, s.end_sample,
                  s.duration_samples) for s in silences])
    except sqlite3.Error as exc:
        raise DatabaseError(f"cannot write run: {exc}") from exc
    return run_id


def load_run(conn, run_id):
    try:
        row = conn.execute(
            "SELECT id, sha256, source_path, sample_rate, total_samples,"
            " params_json, noise_floor_dbfs, threshold_dbfs"
            " FROM runs WHERE id=?", (run_id,)).fetchone()
        if row is None:
            return None
        seg_rows = conn.execute(
            "SELECT ordinal, start_sample, end_sample, fade_in_samples,"
            " fade_out_samples FROM segments WHERE run_id=?"
            " ORDER BY ordinal", (run_id,)).fetchall()
        sil_rows = conn.execute(
            "SELECT ordinal, start_sample, end_sample, duration_samples"
            " FROM silences WHERE run_id=? ORDER BY ordinal", (run_id,)).fetchall()
    except sqlite3.Error as exc:
        raise DatabaseError(f"cannot read run {run_id}: {exc}") from exc
    seg_keys = ("ordinal", "start_sample", "end_sample",
                "fade_in_samples", "fade_out_samples")
    sil_keys = ("ordinal", "start_sample", "end_sample", "duration_samples")
    return {
        "run_id": row[0], "sha256": row[1], "source_path": row[2],
        "sample_rate": row[3], "total_samples": row[4],
        "params": json.loads(row[5]),
        "noise_floor_dbfs": row[6], "threshold_dbfs": row[7],
        "segments": [dict(zip(seg_keys, r)) for r in seg_rows],
        "silences": [dict(zip(sil_keys, r)) for r in sil_rows],
    }
