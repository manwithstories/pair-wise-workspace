"""SQLite checkpoint index: per-chunk input checksum, output offset/length,
output checksum and transaction id. Recovery validates committed chunks and
rolls back the first corrupt chunk plus everything after it.
"""

from __future__ import annotations

import hashlib
import sqlite3
from dataclasses import dataclass
from pathlib import Path

from .errors import MediaBatchError

_SCHEMA = """
CREATE TABLE IF NOT EXISTS chunks (
    seq INTEGER PRIMARY KEY,
    input_sha256 TEXT NOT NULL,
    output_offset INTEGER NOT NULL,
    output_length INTEGER NOT NULL,
    output_sha256 TEXT NOT NULL,
    txn_id INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'committed'
);
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
"""


@dataclass
class ChunkRecord:
    seq: int
    input_sha256: str
    output_offset: int
    output_length: int
    output_sha256: str
    txn_id: int


class CheckpointIndex:
    def __init__(self, db_path: str | Path):
        self.path = Path(db_path)
        try:
            self._db = sqlite3.connect(self.path)
            self._db.execute("PRAGMA journal_mode=WAL")
            self._db.executescript(_SCHEMA)
        except sqlite3.DatabaseError as exc:
            raise MediaBatchError("index_corrupt",
                                  f"checkpoint index {self.path} is corrupt: {exc}",
                                  str(self.path), 0)

    def close(self) -> None:
        self._db.close()

    def _execute(self, *args):
        try:
            return self._db.execute(*args)
        except sqlite3.DatabaseError as exc:
            raise MediaBatchError("index_corrupt",
                                  f"checkpoint index error: {exc}", str(self.path), 0)

    def get_meta(self, key: str, default: str | None = None) -> str | None:
        row = self._execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return row[0] if row else default

    def set_meta(self, key: str, value: str) -> None:
        with self._db:
            self._db.execute("INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)",
                             (key, value))

    def next_txn_id(self) -> int:
        row = self._execute("SELECT COALESCE(MAX(txn_id), 0) FROM chunks").fetchone()
        return row[0] + 1

    def commit_chunk(self, rec: ChunkRecord) -> None:
        """Atomically record one committed chunk (single transaction)."""
        try:
            with self._db:
                self._db.execute(
                    "INSERT OR REPLACE INTO chunks"
                    "(seq, input_sha256, output_offset, output_length, output_sha256, txn_id, status)"
                    " VALUES(?, ?, ?, ?, ?, ?, 'committed')",
                    (rec.seq, rec.input_sha256, rec.output_offset,
                     rec.output_length, rec.output_sha256, rec.txn_id))
        except sqlite3.DatabaseError as exc:
            raise MediaBatchError("index_corrupt",
                                  f"failed to commit chunk {rec.seq}: {exc}",
                                  str(self.path), 0)

    def committed_chunks(self) -> list[ChunkRecord]:
        rows = self._execute(
            "SELECT seq, input_sha256, output_offset, output_length, output_sha256, txn_id"
            " FROM chunks WHERE status='committed' ORDER BY seq").fetchall()
        return [ChunkRecord(*r) for r in rows]

    def rollback_from(self, seq: int) -> int:
        """Delete chunk `seq` and everything after it. Returns rows removed."""
        with self._db:
            cur = self._db.execute("DELETE FROM chunks WHERE seq >= ?", (seq,))
            return cur.rowcount

    def chunk_count(self) -> int:
        return self._execute("SELECT COUNT(*) FROM chunks").fetchone()[0]


def sha256_file_region(path: str | Path, offset: int, length: int) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        fh.seek(offset)
        remaining = length
        while remaining > 0:
            data = fh.read(min(1 << 20, remaining))
            if not data:
                break
            h.update(data)
            remaining -= len(data)
    return h.hexdigest()


def verify_chunks(records: list[ChunkRecord], output_path: str | Path
                  ) -> tuple[list[ChunkRecord], ChunkRecord | None]:
    """Verify committed chunks against the output file, in order.

    Returns (valid_records, first_corrupt_record_or_None). Verification stops
    at the first mismatch: that chunk and all later ones must be recomputed.
    """
    valid: list[ChunkRecord] = []
    for rec in records:
        digest = sha256_file_region(output_path, rec.output_offset, rec.output_length)
        if digest != rec.output_sha256:
            return valid, rec
        valid.append(rec)
    return valid, None
