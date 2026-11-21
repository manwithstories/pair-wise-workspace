"""Transactional local SQLite index: manifests, parameters and checkpoints.

Every processing chunk is written in one SQLite transaction so an
interruption never leaves a partial chunk visible.  The index records:

* ``inputs``      – one row per source file with a content hash and the
  processing parameters it was run with.
* ``segments``    – finalized segment boundaries on the shared timeline.
* ``silence``     – detected silence regions.
* ``fingerprints`` – per-chunk content hashes.
* ``errors``      – isolated recoverable error regions.
* ``checkpoints`` – the last durable position per input.

Resume safety rests on a content hash plus a canonical parameter digest:
a resume request only proceeds when both match the stored run.  Any
mismatch raises :class:`~archive_media.errors.FatalMediaError` rather than
appending incompatible results.
"""

from __future__ import annotations

import hashlib
import json
import sqlite3
from dataclasses import dataclass
from pathlib import Path

from .errors import E_CHECKPOINT_INCOMPATIBLE, FatalMediaError

SCHEMA = """
CREATE TABLE IF NOT EXISTS inputs (
    input_path TEXT PRIMARY KEY,
    content_hash TEXT NOT NULL,
    params_digest TEXT NOT NULL,
    params_json TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at REAL NOT NULL DEFAULT (julianday('now'))
);
CREATE TABLE IF NOT EXISTS checkpoints (
    input_path TEXT PRIMARY KEY,
    chunk_index INTEGER NOT NULL,
    last_time_s REAL NOT NULL,
    resumed INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY(input_path) REFERENCES inputs(input_path)
);
CREATE TABLE IF NOT EXISTS segments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    input_path TEXT NOT NULL,
    segment_id INTEGER NOT NULL,
    track TEXT NOT NULL,
    payload_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS silence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    input_path TEXT NOT NULL,
    payload_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS fingerprints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    input_path TEXT NOT NULL,
    kind TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS errors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    input_path TEXT NOT NULL,
    payload_json TEXT NOT NULL
);
"""


def file_content_hash(path: str, *, chunk: int = 1 << 20) -> str:
    """Stable BLAKE2b content hash, streamed to avoid full-file buffering."""
    digest = hashlib.blake2b(digest_size=20)
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(chunk), b""):
            digest.update(block)
    return digest.hexdigest()


def canonical_params_digest(params: dict) -> str:
    """Order-independent canonical digest of processing parameters."""
    blob = json.dumps(params, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.blake2b(blob, digest_size=16).hexdigest()


@dataclass
class Checkpoint:
    chunk_index: int
    last_time_s: float
    resumed: bool


class MediaIndex:
    """Owns the SQLite connection and all durable writes."""

    def __init__(self, db_path: str) -> None:
        Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        self.db_path = db_path
        self.conn = sqlite3.connect(db_path)
        self.conn.row_factory = sqlite3.Row
        self.conn.executescript(SCHEMA)
        self.conn.commit()

    def close(self) -> None:
        self.conn.close()

    def __enter__(self) -> "MediaIndex":
        return self

    def __exit__(self, *exc_info) -> None:
        self.close()

    # -- run registration / resume --------------------------------------

    def register_or_verify(
        self, input_path: str, content_hash: str, params: dict
    ) -> Checkpoint:
        """Verify compatibility and return the resume checkpoint.

        A brand-new input is registered fresh.  An existing input resumes
        only when its content hash and parameter digest both match.
        """
        params_digest = canonical_params_digest(params)
        row = self.conn.execute(
            "SELECT content_hash, params_digest FROM inputs WHERE input_path = ?",
            (input_path,),
        ).fetchone()
        if row is None:
            self.conn.execute(
                "INSERT INTO inputs(input_path, content_hash, params_digest, params_json, status)"
                " VALUES (?, ?, ?, ?, 'running')",
                (input_path, content_hash, params_digest, json.dumps(params, sort_keys=True)),
            )
            self.conn.execute(
                "INSERT INTO checkpoints(input_path, chunk_index, last_time_s, resumed)"
                " VALUES (?, 0, 0.0, 0)",
                (input_path,),
            )
            self.conn.commit()
            return Checkpoint(0, 0.0, False)
        if row["content_hash"] != content_hash:
            raise FatalMediaError(
                E_CHECKPOINT_INCOMPATIBLE,
                f"input {input_path} changed since checkpoint; refusing to resume",
            )
        if row["params_digest"] != params_digest:
            raise FatalMediaError(
                E_CHECKPOINT_INCOMPATIBLE,
                f"processing parameters for {input_path} differ from checkpoint",
            )
        cp = self.conn.execute(
            "SELECT chunk_index, last_time_s, resumed FROM checkpoints WHERE input_path = ?",
            (input_path,),
        ).fetchone()
        return Checkpoint(cp["chunk_index"], cp["last_time_s"], bool(cp["resumed"]))

    def mark_resumed(self, input_path: str) -> None:
        self.conn.execute(
            "UPDATE checkpoints SET resumed = 1 WHERE input_path = ?", (input_path,)
        )
        self.conn.commit()

    # -- chunk transaction ----------------------------------------------

    def commit_chunk(
        self,
        input_path: str,
        chunk_index: int,
        last_time_s: float,
        *,
        segments: list[dict] | None = None,
        silence: list[dict] | None = None,
        fingerprints: list[tuple[str, int, str]] | None = None,
        errors: list[dict] | None = None,
    ) -> None:
        """Atomically persist one chunk and advance its checkpoint."""
        try:
            with self.conn:  # single transaction
                for seg in segments or []:
                    self.conn.execute(
                        "INSERT INTO segments(input_path, segment_id, track, payload_json)"
                        " VALUES (?, ?, ?, ?)",
                        (input_path, seg["segment_id"], seg["track"], json.dumps(seg, sort_keys=True)),
                    )
                for region in silence or []:
                    self.conn.execute(
                        "INSERT INTO silence(input_path, payload_json) VALUES (?, ?)",
                        (input_path, json.dumps(region, sort_keys=True)),
                    )
                for kind, ordinal, digest in fingerprints or []:
                    self.conn.execute(
                        "INSERT INTO fingerprints(input_path, kind, ordinal, hash)"
                        " VALUES (?, ?, ?, ?)",
                        (input_path, kind, ordinal, digest),
                    )
                for err in errors or []:
                    self.conn.execute(
                        "INSERT INTO errors(input_path, payload_json) VALUES (?, ?)",
                        (input_path, json.dumps(err, sort_keys=True)),
                    )
                self.conn.execute(
                    "UPDATE checkpoints SET chunk_index = ?, last_time_s = ?"
                    " WHERE input_path = ?",
                    (chunk_index, last_time_s, input_path),
                )
        except sqlite3.Error as exc:
            # Import lazily to avoid a circular import at module load.
            from .errors import E_INDEX_WRITE

            raise FatalMediaError(
                E_INDEX_WRITE, f"index write failed: {exc}", time_s=last_time_s
            ) from exc

    def mark_status(self, input_path: str, status: str) -> None:
        with self.conn:
            self.conn.execute(
                "UPDATE inputs SET status = ? WHERE input_path = ?", (status, input_path)
            )

    # -- read-back -------------------------------------------------------

    def load_segments(self, input_path: str) -> list[dict]:
        rows = self.conn.execute(
            "SELECT payload_json FROM segments WHERE input_path = ? ORDER BY id",
            (input_path,),
        ).fetchall()
        return [json.loads(r["payload_json"]) for r in rows]

    def load_silence(self, input_path: str) -> list[dict]:
        rows = self.conn.execute(
            "SELECT payload_json FROM silence WHERE input_path = ? ORDER BY id",
            (input_path,),
        ).fetchall()
        return [json.loads(r["payload_json"]) for r in rows]

    def load_fingerprint_hashes(self, input_path: str) -> list[str]:
        rows = self.conn.execute(
            "SELECT hash FROM fingerprints WHERE input_path = ? ORDER BY id",
            (input_path,),
        ).fetchall()
        return [r["hash"] for r in rows]

    def load_errors(self, input_path: str) -> list[dict]:
        rows = self.conn.execute(
            "SELECT payload_json FROM errors WHERE input_path = ? ORDER BY id",
            (input_path,),
        ).fetchall()
        return [json.loads(r["payload_json"]) for r in rows]
