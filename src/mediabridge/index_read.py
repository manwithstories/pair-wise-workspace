"""Read-only index access for verify and resume validation.

Kept separate from the writer so the read path can be audited on its own:
opening an index validates its schema, version and completion state before
any row is trusted.
"""

from __future__ import annotations

import json
import sqlite3
from dataclasses import dataclass
from pathlib import Path

from .errors import IndexIncompatibleError
from .index_types import INDEX_VERSION


@dataclass
class IndexInfo:
    version: int
    tool_version: str
    source: dict
    profile: dict
    output: dict
    commit_seq: int
    chunk_count: int
    sample_count: int
    damage_count: int


class IndexReader:
    """Read-only view used by ``verify`` and by resume validation."""

    def __init__(self, path: Path, *, require_complete: bool = True):
        self.path = Path(path)
        if not self.path.exists():
            raise IndexIncompatibleError(f"index file does not exist: {self.path}",
                                         context={"path": str(self.path)})
        try:
            self.connection = sqlite3.connect(f"file:{self.path}?mode=ro", uri=True)
            self.connection.row_factory = sqlite3.Row
        except sqlite3.Error as exc:
            raise IndexIncompatibleError(f"cannot open index: {exc}") from exc
        try:
            self._validate(require_complete)
        except Exception:
            self.connection.close()
            raise

    def _validate(self, require_complete: bool) -> None:
        try:
            tables = {row[0] for row in self.connection.execute(
                "SELECT name FROM sqlite_master WHERE type='table'")}
        except sqlite3.DatabaseError as exc:
            raise IndexIncompatibleError(f"index is not readable SQLite: {exc}") from exc
        required = {"meta", "tracks", "chunks", "samples", "damage"}
        missing = required - tables
        if missing:
            raise IndexIncompatibleError(
                f"index is missing tables {sorted(missing)}",
                context={"path": str(self.path)},
            )
        meta = self.meta()
        version = int(meta.get("index_version", 0))
        if version != INDEX_VERSION:
            raise IndexIncompatibleError(
                f"index version {version} is not supported (expected {INDEX_VERSION})",
                context={"path": str(self.path), "index_version": version},
            )
        if require_complete and meta.get("state") not in ("complete", None):
            raise IndexIncompatibleError(
                f"index state {meta.get('state')!r} is not usable",
                context={"path": str(self.path), "state": meta.get("state")},
            )
        self._meta = meta

    def meta(self) -> dict:
        rows = self.connection.execute("SELECT key, value FROM meta").fetchall()
        return {row["key"]: row["value"] for row in rows}

    def info(self) -> IndexInfo:
        meta = self._meta
        return IndexInfo(
            version=int(meta.get("index_version", 0)),
            tool_version=meta.get("tool_version", ""),
            source=json.loads(meta.get("source", "{}")),
            profile=json.loads(meta.get("profile", "{}")),
            output=json.loads(meta.get("output", "{}")),
            commit_seq=int(meta.get("commit_seq", 0)),
            chunk_count=self.connection.execute("SELECT COUNT(*) FROM chunks").fetchone()[0],
            sample_count=self.connection.execute("SELECT COUNT(*) FROM samples").fetchone()[0],
            damage_count=self.connection.execute("SELECT COUNT(*) FROM damage").fetchone()[0],
        )

    def tracks(self) -> list[dict]:
        return [dict(row) for row in self.connection.execute(
            "SELECT * FROM tracks ORDER BY track_index")]

    def chunks(self) -> list[dict]:
        return [dict(row) for row in self.connection.execute(
            "SELECT * FROM chunks ORDER BY chunk_index")]

    def last_committed_chunk(self) -> dict | None:
        row = self.connection.execute(
            "SELECT * FROM chunks ORDER BY chunk_index DESC LIMIT 1").fetchone()
        return dict(row) if row else None

    def committed_bytes(self) -> int:
        row = self.connection.execute(
            "SELECT file_offset, file_length FROM chunks ORDER BY chunk_index DESC LIMIT 1").fetchone()
        if not row:
            return 0
        return int(row["file_offset"]) + int(row["file_length"])

    def samples(self) -> list[dict]:
        return [dict(row) for row in self.connection.execute("SELECT * FROM samples ORDER BY id")]

    def keyframes(self, track_index: int | None = None) -> list[dict]:
        if track_index is None:
            rows = self.connection.execute(
                "SELECT * FROM samples WHERE keyframe=1 ORDER BY time_seconds")
        else:
            rows = self.connection.execute(
                "SELECT * FROM samples WHERE keyframe=1 AND track_index=? ORDER BY time_seconds",
                (track_index,))
        return [dict(row) for row in rows]

    def damage(self) -> list[dict]:
        return [dict(row) for row in self.connection.execute("SELECT * FROM damage ORDER BY id")]

    def chunk_digest(self, chunk_index: int) -> str | None:
        row = self.connection.execute(
            "SELECT rows_sha256 FROM chunks WHERE chunk_index=?", (chunk_index,)).fetchone()
        return row[0] if row else None

    def close(self) -> None:
        self.connection.close()

    def __enter__(self) -> "IndexReader":
        return self

    def __exit__(self, *exc) -> None:
        self.close()


def connect_for_resume(path: Path) -> sqlite3.Connection:
    """Open an existing index for appending commit records (resume path)."""
    path = Path(path)
    if not path.exists():
        raise IndexIncompatibleError(f"cannot resume without an index: {path}")
    connection = sqlite3.connect(str(path))
    connection.row_factory = sqlite3.Row
    return connection
