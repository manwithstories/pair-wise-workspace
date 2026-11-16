"""sqlite persistence with parameter snapshots and idempotent upserts."""

from __future__ import annotations

import sqlite3
from pathlib import Path

from .config import Params
from .errors import DatabaseError
from .refine import Segment
from .util import safe_resolve

_SCHEMA = """
CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    params_json TEXT NOT NULL,
    sample_rate INTEGER NOT NULL,
    channels INTEGER NOT NULL,
    bits_per_sample INTEGER NOT NULL,
    total_frames INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS segments (
    file_id INTEGER NOT NULL,
    seg_index INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample INTEGER NOT NULL,
    start_frame INTEGER NOT NULL,
    end_frame INTEGER NOT NULL,
    fade_in_ms REAL,
    fade_out_ms REAL,
    PRIMARY KEY (file_id, seg_index),
    FOREIGN KEY (file_id) REFERENCES files(id)
);
CREATE INDEX IF NOT EXISTS idx_files_hash ON files(sha256, params_json);
"""


class Store:
    def __init__(self, db_path: str | Path):
        self.path = safe_resolve(db_path) if Path(db_path).is_absolute() else Path(db_path)
        try:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            self.conn = sqlite3.connect(str(self.path))
            self.conn.row_factory = sqlite3.Row
            self.conn.executescript(_SCHEMA)
            self.conn.commit()
        except sqlite3.Error as exc:
            raise DatabaseError(f"cannot open database {db_path}: {exc}") from exc

    def close(self) -> None:
        try:
            self.conn.close()
        except sqlite3.Error as exc:
            raise DatabaseError(str(exc)) from exc

    def __enter__(self) -> "Store":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def upsert_analysis(self, path: Path, sha256: str, params: Params,
                        info, segments: list[Segment]) -> tuple[int, bool]:
        """Insert or reuse the analysis row; old parameter rows are superseded."""
        params_json = params.canonical_json()
        try:
            cur = self.conn.execute(
                "SELECT id FROM files WHERE sha256 = ? AND params_json = ? AND status = 'active'",
                (sha256, params_json))
            row = cur.fetchone()
            if row is not None:
                return int(row["id"]), False
            self.conn.execute(
                "UPDATE files SET status = 'superseded' WHERE sha256 = ?", (sha256,))
            cur = self.conn.execute(
                """INSERT INTO files (path, sha256, params_json, sample_rate,
                   channels, bits_per_sample, total_frames, status)
                   VALUES (?, ?, ?, ?, ?, ?, ?, 'active')""",
                (str(path), sha256, params_json, info.sample_rate, info.channels,
                 info.bits_per_sample, info.total_frames))
            file_id = int(cur.lastrowid)
            self.conn.executemany(
                """INSERT INTO segments (file_id, seg_index, start_sample,
                   end_sample, start_frame, end_frame, fade_in_ms, fade_out_ms)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?)""",
                [(file_id, idx, seg.start_sample, seg.end_sample, seg.start_frame,
                  seg.end_frame, seg.fade_in_ms, seg.fade_out_ms)
                 for idx, seg in enumerate(segments)])
            self.conn.commit()
            return file_id, True
        except sqlite3.Error as exc:
            self.conn.rollback()
            raise DatabaseError(f"upsert failed for {path}: {exc}") from exc

    def active_files(self) -> list[sqlite3.Row]:
        return list(self.conn.execute(
            "SELECT * FROM files WHERE status = 'active' ORDER BY path"))

    def get_file(self, file_id: int) -> sqlite3.Row:
        row = self.conn.execute("SELECT * FROM files WHERE id = ?", (file_id,)).fetchone()
        if row is None:
            raise DatabaseError(f"unknown file id {file_id}")
        return row

    def segments_for(self, file_id: int) -> list[sqlite3.Row]:
        return list(self.conn.execute(
            "SELECT * FROM segments WHERE file_id = ? ORDER BY seg_index", (file_id,)))

    def query_segments(self, file_glob: str | None = None,
                       min_silence_ms: float | None = None,
                       start_ms: float | None = None,
                       end_ms: float | None = None) -> list[dict]:
        out: list[dict] = []
        for frow in self.active_files():
            if file_glob and Path(frow["path"]).match(file_glob) is False:
                continue
            rows = self.segments_for(frow["id"])
            rate = frow["sample_rate"]
            gap_limit = (min_silence_ms or 0.0) / 1000.0 * rate
            for idx, row in enumerate(rows):
                gap_before = 0
                if idx:
                    gap_before = row["start_sample"] - rows[idx - 1]["end_sample"]
                if min_silence_ms is not None and gap_before < gap_limit:
                    continue
                start_ms_actual = row["start_sample"] / rate * 1000.0
                end_ms_actual = row["end_sample"] / rate * 1000.0
                if start_ms is not None and end_ms_actual < start_ms:
                    continue
                if end_ms is not None and start_ms_actual > end_ms:
                    continue
                out.append({"file": frow["path"], "seg_index": row["seg_index"],
                            "start_sample": row["start_sample"],
                            "end_sample": row["end_sample"],
                            "start_ms": round(start_ms_actual, 2),
                            "end_ms": round(end_ms_actual, 2),
                            "silence_before_samples": gap_before})
        return out
