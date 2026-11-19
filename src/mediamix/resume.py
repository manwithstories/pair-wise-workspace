"""Resumable pipeline state: input fingerprints, stage checkpoints, frame log.

State lives in a single SQLite database written atomically (temp file +
``os.replace`` at close). Checkpoints only become durable at stage boundaries,
each guarded by the fingerprint of every input file and by the exact pipeline
parameters. A resume whose inputs or parameters changed is rejected with
:class:`CheckpointConflictError` instead of silently mixing old and new work.
"""

from __future__ import annotations

import hashlib
import json
import os
import sqlite3
from dataclasses import dataclass
from pathlib import Path

from .model import CheckpointConflictError

SCHEMA = """
CREATE TABLE IF NOT EXISTS fingerprints (
    path TEXT PRIMARY KEY,
    sha256 TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS checkpoints (
    stage TEXT PRIMARY KEY,
    payload TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS frames (
    name TEXT PRIMARY KEY,
    tick INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    width INTEGER NOT NULL,
    height INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
"""

STAGES = ("probe", "clock", "timeline", "render", "validate")


def sha256_file(path: str | Path, chunk_size: int = 1 << 20) -> str:
    """Stream-hash a file without loading it into memory."""
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(chunk_size), b""):
            h.update(chunk)
    return h.hexdigest()


def fingerprint_params(params: dict) -> str:
    """Stable hash of pipeline parameters (sorted JSON, no whitespace drift)."""
    blob = json.dumps(params, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()


@dataclass
class FrameEntry:
    name: str
    tick: int
    sha256: str
    width: int
    height: int


class CheckpointStore:
    """Transaction-safe wrapper around the on-disk checkpoint database."""

    def __init__(self, db_path: str | Path):
        self.db_path = Path(db_path)
        self._conn: sqlite3.Connection | None = None

    # -- lifecycle ---------------------------------------------------------
    def open(self) -> "CheckpointStore":
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        # NORMAL sync + WAL: per-statement commits stay cheap while the final
        # close still flushes durably; bulk frame marking runs in one txn.
        self._conn = sqlite3.connect(self.db_path)
        self._conn.row_factory = sqlite3.Row
        self._conn.execute("PRAGMA journal_mode=WAL")
        self._conn.execute("PRAGMA synchronous=NORMAL")
        self._conn.executescript(SCHEMA)
        self._conn.commit()
        return self

    def close(self) -> None:
        if self._conn is not None:
            self._conn.commit()
            self._conn.close()
            self._conn = None

    def __enter__(self) -> "CheckpointStore":
        return self.open()

    def __exit__(self, *exc) -> None:
        self.close()

    @property
    def conn(self) -> sqlite3.Connection:
        assert self._conn is not None, "CheckpointStore must be opened first"
        return self._conn

    # -- fingerprints ------------------------------------------------------
    def expected_fingerprints(self) -> dict[str, str]:
        rows = self.conn.execute("SELECT path, sha256 FROM fingerprints").fetchall()
        return {r["path"]: r["sha256"] for r in rows}

    def set_fingerprints(self, fingerprints: dict[str, str]) -> None:
        with self.conn:
            self.conn.execute("DELETE FROM fingerprints")
            self.conn.executemany(
                "INSERT INTO fingerprints(path, sha256) VALUES (?, ?)",
                sorted(fingerprints.items()),
            )

    def verify_fingerprints(self, current: dict[str, str]) -> None:
        """Reject resume when any input file changed or disappeared."""
        stored = self.expected_fingerprints()
        if stored != current:
            missing = sorted(set(stored) - set(current))
            changed = sorted(
                p for p in stored.keys() & current.keys() if stored[p] != current[p]
            )
            added = sorted(set(current) - set(stored))
            raise CheckpointConflictError(
                "stale checkpoint: input fingerprint mismatch "
                f"(changed={changed}, missing={missing}, added={added})"
            )

    # -- parameters --------------------------------------------------------
    def store_params(self, params: dict) -> None:
        with self.conn:
            self.conn.execute(
                "INSERT OR REPLACE INTO meta(key, value) VALUES ('params', ?)",
                (fingerprint_params(params),),
            )

    def verify_params(self, params: dict) -> None:
        row = self.conn.execute(
            "SELECT value FROM meta WHERE key = 'params'"
        ).fetchone()
        current = fingerprint_params(params)
        if row is None:
            raise CheckpointConflictError("checkpoint has no recorded parameters")
        if row["value"] != current:
            raise CheckpointConflictError(
                f"pipeline parameters changed: stored={row['value']} current={current}"
            )

    # -- stage checkpoints -------------------------------------------------
    def save_stage(self, stage: str, payload: dict) -> None:
        if stage not in STAGES:
            raise ValueError(f"unknown stage {stage!r}")
        with self.conn:
            self.conn.execute(
                "INSERT OR REPLACE INTO checkpoints(stage, payload) VALUES (?, ?)",
                (stage, json.dumps(payload, sort_keys=True)),
            )

    def load_stage(self, stage: str) -> dict | None:
        row = self.conn.execute(
            "SELECT payload FROM checkpoints WHERE stage = ?", (stage,)
        ).fetchone()
        return json.loads(row["payload"]) if row is not None else None

    def stages_done(self) -> list[str]:
        rows = self.conn.execute("SELECT stage FROM checkpoints").fetchall()
        return [r["stage"] for r in rows]

    def reset(self) -> None:
        with self.conn:
            self.conn.execute("DELETE FROM checkpoints")
            self.conn.execute("DELETE FROM frames")

    # -- rendered frame ledger --------------------------------------------
    def mark_frame(self, entry: FrameEntry) -> None:
        with self.conn:
            self.conn.execute(
                "INSERT OR REPLACE INTO frames(name, tick, sha256, width, height)"
                " VALUES (?, ?, ?, ?, ?)",
                (entry.name, entry.tick, entry.sha256, entry.width, entry.height),
            )

    def mark_frames_bulk(self, entries: list[FrameEntry]) -> None:
        """Record many frames in a single transaction (one commit, one fsync)."""
        if not entries:
            return
        with self.conn:
            self.conn.executemany(
                "INSERT OR REPLACE INTO frames(name, tick, sha256, width, height)"
                " VALUES (?, ?, ?, ?, ?)",
                [(e.name, e.tick, e.sha256, e.width, e.height) for e in entries],
            )

    def known_frames(self) -> dict[str, FrameEntry]:
        rows = self.conn.execute("SELECT * FROM frames").fetchall()
        return {
            r["name"]: FrameEntry(
                name=r["name"], tick=r["tick"], sha256=r["sha256"],
                width=r["width"], height=r["height"],
            )
            for r in rows
        }

    def frame_count(self) -> int:
        return int(self.conn.execute("SELECT COUNT(*) AS c FROM frames").fetchone()["c"])


def atomic_write_bytes(path: Path, data: bytes, *, fsync: bool = False) -> None:
    """Write ``data`` to ``path`` atomically via a sibling temp file.

    The ``os.replace`` rename guarantees readers never see a partial file.
    ``fsync`` defaults off because the checkpoint DB carries durability and
    bulk renders issue thousands of these writes; callers needing power-loss
    durability can opt in.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    with open(tmp, "wb") as fh:
        fh.write(data)
        if fsync:
            fh.flush()
            os.fsync(fh.fileno())
    tmp.replace(path)


def atomic_write_json(path: Path, payload: dict) -> None:
    atomic_write_bytes(
        path, json.dumps(payload, indent=2, sort_keys=True).encode("utf-8")
    )
