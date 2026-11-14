"""SQLite storage for detection runs.

A run is uniquely identified by (input_path, params_hash): re-running
detect with the same input and parameters replaces the stored segments
in place, so repeated execution is idempotent and never duplicates runs.
"""
from __future__ import annotations

import hashlib
import json
import os
import sqlite3
import time

_SCHEMA = """
CREATE TABLE IF NOT EXISTS runs (
    id INTEGER PRIMARY KEY,
    input_path TEXT NOT NULL,
    params_hash TEXT NOT NULL,
    params_json TEXT NOT NULL,
    sample_rate INTEGER NOT NULL,
    n_samples INTEGER NOT NULL,
    n_segments INTEGER NOT NULL,
    silence_total_samples INTEGER NOT NULL,
    silence_total_sec REAL NOT NULL,
    created TEXT NOT NULL,
    UNIQUE (input_path, params_hash)
);
CREATE TABLE IF NOT EXISTS segments (
    run_id INTEGER NOT NULL REFERENCES runs(id),
    idx INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample INTEGER NOT NULL,
    PRIMARY KEY (run_id, idx)
);
CREATE TABLE IF NOT EXISTS silences (
    run_id INTEGER NOT NULL REFERENCES runs(id),
    idx INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample INTEGER NOT NULL,
    PRIMARY KEY (run_id, idx)
);
"""


def connect(db_path: str) -> sqlite3.Connection:
    parent = os.path.dirname(os.path.abspath(db_path))
    os.makedirs(parent, exist_ok=True)
    conn = sqlite3.connect(db_path)
    conn.executescript(_SCHEMA)
    return conn


def params_hash(params: dict) -> str:
    blob = json.dumps(params, sort_keys=True, separators=(",", ":"))
    return hashlib.sha1(blob.encode("utf-8")).hexdigest()


def save_run(db_path: str, input_path: str, params: dict, sample_rate: int,
             n_samples: int, segments: list[tuple[int, int]],
             silences: list[tuple[int, int]],
             silence_total_samples: int,
             silence_total_sec: float) -> int:
    ph = params_hash(params)
    conn = connect(db_path)
    try:
        with conn:
            row = conn.execute(
                "SELECT id FROM runs WHERE input_path=? AND params_hash=?",
                (input_path, ph)).fetchone()
            if row is not None:
                run_id = row[0]
                conn.execute("DELETE FROM segments WHERE run_id=?", (run_id,))
                conn.execute("DELETE FROM silences WHERE run_id=?", (run_id,))
                conn.execute(
                    "UPDATE runs SET params_json=?, sample_rate=?, n_samples=?,"
                    " n_segments=?, silence_total_samples=?,"
                    " silence_total_sec=?, created=? WHERE id=?",
                    (json.dumps(params, sort_keys=True), sample_rate,
                     n_samples, len(segments), silence_total_samples,
                     silence_total_sec, time.strftime("%Y-%m-%dT%H:%M:%S"),
                     run_id))
            else:
                cur = conn.execute(
                    "INSERT INTO runs (input_path, params_hash, params_json,"
                    " sample_rate, n_samples, n_segments,"
                    " silence_total_samples, silence_total_sec, created)"
                    " VALUES (?,?,?,?,?,?,?,?,?)",
                    (input_path, ph, json.dumps(params, sort_keys=True),
                     sample_rate, n_samples, len(segments),
                     silence_total_samples, silence_total_sec,
                     time.strftime("%Y-%m-%dT%H:%M:%S")))
                run_id = cur.lastrowid
            conn.executemany(
                "INSERT INTO segments (run_id, idx, start_sample, end_sample)"
                " VALUES (?,?,?,?)",
                [(run_id, i, s, e) for i, (s, e) in enumerate(segments)])
            conn.executemany(
                "INSERT INTO silences (run_id, idx, start_sample, end_sample)"
                " VALUES (?,?,?,?)",
                [(run_id, i, s, e) for i, (s, e) in enumerate(silences)])
    finally:
        conn.close()
    return run_id


def _load_run(conn: sqlite3.Connection, row) -> dict:
    run_id = row[0]
    segments = conn.execute(
        "SELECT idx, start_sample, end_sample FROM segments"
        " WHERE run_id=? ORDER BY idx", (run_id,)).fetchall()
    silences = conn.execute(
        "SELECT idx, start_sample, end_sample FROM silences"
        " WHERE run_id=? ORDER BY idx", (run_id,)).fetchall()
    return {
        "id": run_id,
        "input_path": row[1],
        "params_hash": row[2],
        "params": json.loads(row[3]),
        "sample_rate": row[4],
        "n_samples": row[5],
        "n_segments": row[6],
        "silence_total_samples": row[7],
        "silence_total_sec": row[8],
        "created": row[9],
        "segments": [{"idx": i, "start_sample": s, "end_sample": e}
                     for i, s, e in segments],
        "silences": [{"idx": i, "start_sample": s, "end_sample": e}
                     for i, s, e in silences],
    }


def latest_run(db_path: str) -> dict | None:
    if not os.path.exists(db_path):
        return None
    conn = connect(db_path)
    try:
        row = conn.execute(
            "SELECT id, input_path, params_hash, params_json, sample_rate,"
            " n_samples, n_segments, silence_total_samples,"
            " silence_total_sec, created FROM runs ORDER BY id DESC LIMIT 1"
        ).fetchone()
        return _load_run(conn, row) if row else None
    finally:
        conn.close()


def count_runs(db_path: str) -> int:
    if not os.path.exists(db_path):
        return 0
    conn = connect(db_path)
    try:
        return conn.execute("SELECT COUNT(*) FROM runs").fetchone()[0]
    finally:
        conn.close()
