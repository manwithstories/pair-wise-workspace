"""Atomic chunk commits, checkpoint file and the local SQLite index.

Crash-safety rules implemented here:

* A chunk is written to ``<name>.part``, fsynced, and only then renamed onto
  its final name.  A reader never sees a half-written chunk.
* A chunk (and its audio/video payload) is only recorded in the manifest once
  both the file rename and the index insert succeeded.  The manifest therefore
  never lists unverified data.
* ``checkpoint.json`` is rewritten atomically after each commit, so
  ``resume`` can tell which chunks were already durable.
* On resume every committed chunk is re-hashed and decoded.  A chunk whose
  hash or decoded length disagrees with the index is *quarantined* - moved
  aside and reported - instead of being trusted or silently re-used.
"""

from __future__ import annotations

import hashlib
import json
import os
import sqlite3
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .errors import Code, Diagnostic

CHECKPOINT_NAME = "checkpoint.json"
INDEX_NAME = "index.sqlite3"
MANIFEST_NAME = "manifest.json"
PART_SUFFIX = ".part"
QUARANTINE_DIR = "quarantine"


class CheckpointError(RuntimeError):
    def __init__(self, code: Code, message: str, detail: dict | None = None) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.detail = detail or {}

    def diagnostic(self) -> Diagnostic:
        return Diagnostic(self.code, self.message, detail=self.detail)


def _rate_from_seconds(record: "ChunkRecord") -> int:
    """Recover the output rate when a caller did not set ``out_rate``."""
    if record.start_seconds and record.start_sample:
        return int(round(record.start_sample / record.start_seconds))
    if record.end_seconds and record.end_sample:
        return int(round(record.end_sample / record.end_seconds))
    return 0


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def atomic_write_bytes(path: Path, data: bytes) -> None:
    """Write ``data`` to ``path`` with a same-directory atomic rename."""
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + PART_SUFFIX)
    with tmp.open("wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    tmp.replace(path)
    _fsync_dir(path.parent)


def atomic_write_json(path: Path, payload: Any) -> None:
    atomic_write_bytes(
        path, (json.dumps(payload, indent=2, sort_keys=True) + "\n").encode("utf-8")
    )


def atomic_commit_file(tmp: Path, final: Path) -> None:
    """fsync a temporary file and rename it into place."""
    with tmp.open("rb+") as handle:
        handle.flush()
        os.fsync(handle.fileno())
    tmp.replace(final)
    _fsync_dir(final.parent)


def _fsync_dir(directory: Path) -> None:
    try:
        fd = os.open(str(directory), os.O_RDONLY)
    except OSError:  # pragma: no cover - platform dependent
        return
    try:
        os.fsync(fd)
    except OSError:  # pragma: no cover - platform dependent
        pass
    finally:
        os.close(fd)


@dataclass
class ChunkRecord:
    """Durable view of one committed chunk (mirrors the SQLite row)."""

    index: int
    name: str
    sha256: str
    size_bytes: int
    samples: int
    start_sample: int
    end_sample: int
    source_start: int
    source_end: int
    actual_kbps: float
    peak_dbfs: float | None
    loudness: float | None
    clipped_samples: int
    silent: bool
    short: bool
    errors: tuple[str, ...] = ()
    chunk_id: str = ""
    start_seconds: float | None = None
    end_seconds: float | None = None
    source_sequence_start: int | None = None
    source_sequence_end: int | None = None
    timestamp_start: int | None = None
    timestamp_end: int | None = None
    timestamp_wrapped: bool = False
    out_rate: int = 0

    def to_json(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "chunk_id": self.chunk_id or f"chunk-{self.index:05d}",
            "name": self.name,
            "sha256": self.sha256,
            "size_bytes": self.size_bytes,
            "samples": self.samples,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "start_seconds": self.start_seconds,
            "end_seconds": self.end_seconds,
            "source_start": self.source_start,
            "source_end": self.source_end,
            "source_sequence_start": self.source_sequence_start,
            "source_sequence_end": self.source_sequence_end,
            "rtp_timestamp_start": self.timestamp_start,
            "rtp_timestamp_end": self.timestamp_end,
            "rtp_timestamp_wrapped": self.timestamp_wrapped,
            "actual_kbps": round(self.actual_kbps, 4),
            "peak_dbfs": self.peak_dbfs,
            "loudness": self.loudness,
            "clipped_samples": self.clipped_samples,
            "silent": self.silent,
            "short": self.short,
            "errors": list(self.errors),
        }

    @classmethod
    def from_row(cls, row: sqlite3.Row) -> "ChunkRecord":
        return cls(
            index=row["idx"],
            name=row["name"],
            sha256=row["sha256"],
            size_bytes=row["size_bytes"],
            samples=row["samples"],
            start_sample=row["start_sample"],
            end_sample=row["end_sample"],
            source_start=row["source_start"],
            source_end=row["source_end"],
            actual_kbps=row["actual_kbps"],
            peak_dbfs=row["peak_dbfs"],
            loudness=row["loudness"],
            clipped_samples=row["clipped_samples"],
            silent=bool(row["silent"]),
            short=bool(row["short"]),
            errors=tuple(json.loads(row["errors"] or "[]")),
            chunk_id=row["chunk_id"],
            start_seconds=row["start_sample"] / row["out_rate"]
            if row["out_rate"]
            else None,
            end_seconds=row["end_sample"] / row["out_rate"]
            if row["out_rate"]
            else None,
            source_sequence_start=row["source_sequence_start"],
            source_sequence_end=row["source_sequence_end"],
            timestamp_start=row["timestamp_start"],
            timestamp_end=row["timestamp_end"],
            timestamp_wrapped=bool(row["timestamp_wrapped"]),
            out_rate=int(row["out_rate"] or 0),
        )


@dataclass
class CheckpointState:
    version: int = 1
    fingerprint: str = ""
    input_dir: str = ""
    parameters: dict[str, Any] = field(default_factory=dict)
    total_chunks: int = 0
    committed: int = 0
    complete: bool = False
    quarantined: int = 0
    updated_at: str = ""

    def to_json(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "fingerprint": self.fingerprint,
            "input_dir": self.input_dir,
            "parameters": dict(self.parameters),
            "total_chunks": self.total_chunks,
            "committed": self.committed,
            "complete": self.complete,
            "quarantined": self.quarantined,
            "updated_at": self.updated_at,
        }

    @classmethod
    def load(cls, path: Path) -> "CheckpointState":
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise CheckpointError(
                Code.CHECKPOINT_INVALID, f"cannot read checkpoint: {exc}"
            ) from exc
        if not isinstance(data, dict) or data.get("version") != 1:
            raise CheckpointError(
                Code.CHECKPOINT_INVALID, "checkpoint has an unknown version"
            )
        state = cls()
        for key in cls.__dataclass_fields__:  # type: ignore[attr-defined]
            if key in data:
                setattr(state, key, data[key])
        return state


_SCHEMA = """
CREATE TABLE IF NOT EXISTS chunks (
    idx INTEGER PRIMARY KEY,
    chunk_id TEXT NOT NULL,
    name TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    samples INTEGER NOT NULL,
    start_sample INTEGER NOT NULL,
    end_sample INTEGER NOT NULL,
    source_start INTEGER NOT NULL,
    source_end INTEGER NOT NULL,
    actual_kbps REAL NOT NULL,
    peak_dbfs REAL,
    loudness REAL,
    clipped_samples INTEGER NOT NULL,
    silent INTEGER NOT NULL,
    short INTEGER NOT NULL,
    errors TEXT NOT NULL,
    committed_at TEXT NOT NULL,
    source_sequence_start INTEGER,
    source_sequence_end INTEGER,
    timestamp_start INTEGER,
    timestamp_end INTEGER,
    timestamp_wrapped INTEGER NOT NULL DEFAULT 0,
    out_rate INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
"""


class ChunkIndex:
    """Small SQLite index of committed chunks (one row per chunk)."""

    def __init__(self, path: Path) -> None:
        self.path = path
        path.parent.mkdir(parents=True, exist_ok=True)
        self._conn = sqlite3.connect(str(path))
        self._conn.row_factory = sqlite3.Row
        self._conn.executescript(_SCHEMA)
        self._conn.commit()

    def close(self) -> None:
        self._conn.close()

    def __enter__(self) -> "ChunkIndex":
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()

    def set_meta(self, key: str, value: Any) -> None:
        self._conn.execute(
            "INSERT INTO meta(key, value) VALUES(?, ?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, json.dumps(value, sort_keys=True)),
        )
        self._conn.commit()

    def get_meta(self, key: str, default: Any = None) -> Any:
        row = self._conn.execute(
            "SELECT value FROM meta WHERE key=?", (key,)
        ).fetchone()
        if row is None:
            return default
        return json.loads(row["value"])

    def add(self, record: ChunkRecord, *, committed_at: str = "") -> None:
        self._conn.execute(
            "INSERT OR REPLACE INTO chunks VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                record.index,
                record.chunk_id or f"chunk-{record.index:05d}",
                record.name,
                record.sha256,
                record.size_bytes,
                record.samples,
                record.start_sample,
                record.end_sample,
                record.source_start,
                record.source_end,
                record.actual_kbps,
                record.peak_dbfs,
                record.loudness,
                record.clipped_samples,
                int(record.silent),
                int(record.short),
                json.dumps(list(record.errors)),
                committed_at,
                record.source_sequence_start,
                record.source_sequence_end,
                record.timestamp_start,
                record.timestamp_end,
                int(record.timestamp_wrapped),
                int(record.out_rate or 0) or _rate_from_seconds(record),
            ),
        )
        self._conn.commit()

    def remove(self, index: int) -> None:
        self._conn.execute("DELETE FROM chunks WHERE idx=?", (index,))
        self._conn.commit()

    def records(self) -> list[ChunkRecord]:
        rows = self._conn.execute("SELECT * FROM chunks ORDER BY idx").fetchall()
        return [ChunkRecord.from_row(row) for row in rows]

    def get(self, index: int) -> ChunkRecord | None:
        row = self._conn.execute(
            "SELECT * FROM chunks WHERE idx=?", (index,)
        ).fetchone()
        return ChunkRecord.from_row(row) if row else None

    def count(self) -> int:
        row = self._conn.execute("SELECT COUNT(*) AS n FROM chunks").fetchone()
        return int(row["n"])

    def committed_indices(self) -> list[int]:
        rows = self._conn.execute("SELECT idx FROM chunks ORDER BY idx").fetchall()
        return [int(row["idx"]) for row in rows]


@dataclass
class VerificationOutcome:
    """Result of validating committed chunks against the index."""

    verified: list[int] = field(default_factory=list)
    quarantined: list[dict[str, Any]] = field(default_factory=list)
    missing_files: list[int] = field(default_factory=list)
    diagnostics: list[Diagnostic] = field(default_factory=list)
    readable_samples: int = 0

    @property
    def ok(self) -> bool:
        return not self.quarantined and not self.missing_files


def quarantine_chunk(
    out_dir: Path, name: str, index: int, reason: str
) -> Path:
    """Move a suspect chunk out of the delivery directory."""
    source = out_dir / name
    target_dir = out_dir / QUARANTINE_DIR
    target_dir.mkdir(parents=True, exist_ok=True)
    target = target_dir / name
    if source.exists():
        source.replace(target)
    note = target_dir / f"{name}.reason.txt"
    atomic_write_bytes(note, (reason + "\n").encode("utf-8"))
    return target


def verify_committed(
    out_dir: Path,
    index: ChunkIndex,
    *,
    decode: Any | None = None,
) -> VerificationOutcome:
    """Re-hash (and optionally decode) every chunk claimed by the index."""
    from . import codec

    outcome = VerificationOutcome()
    for record in index.records():
        path = out_dir / record.name
        if not path.is_file():
            outcome.missing_files.append(record.index)
            outcome.diagnostics.append(
                Diagnostic(
                    Code.CHUNK_MISSING,
                    "index lists a chunk that is not on disk",
                    str(path),
                    {"index": record.index, "sha256": record.sha256},
                )
            )
            continue
        actual_size = path.stat().st_size
        actual_hash = sha256_file(path)
        if actual_hash != record.sha256 or actual_size != record.size_bytes:
            quarantine_chunk(
                out_dir,
                record.name,
                record.index,
                f"hash/size mismatch: index {record.sha256}/{record.size_bytes}, "
                f"file {actual_hash}/{actual_size}",
            )
            outcome.quarantined.append(
                {
                    "index": record.index,
                    "name": record.name,
                    "expected_sha256": record.sha256,
                    "actual_sha256": actual_hash,
                    "expected_size": record.size_bytes,
                    "actual_size": actual_size,
                }
            )
            outcome.diagnostics.append(
                Diagnostic(
                    Code.CHUNK_HASH_MISMATCH,
                    "chunk content does not match the index",
                    str(path),
                    {"index": record.index},
                )
            )
            continue
        if record.samples != record.end_sample - record.start_sample:
            outcome.diagnostics.append(
                Diagnostic(
                    Code.INDEX_INCONSISTENT,
                    "index sample count disagrees with its own range",
                    str(path),
                    {"index": record.index},
                )
            )
        if decode is not None:
            try:
                decoded = decode(path, record)
            except Exception as exc:  # pragma: no cover - defensive
                quarantine_chunk(out_dir, record.name, record.index, str(exc))
                outcome.quarantined.append(
                    {"index": record.index, "name": record.name, "error": str(exc)}
                )
                outcome.diagnostics.append(
                    Diagnostic(
                        Code.INDEX_INCONSISTENT,
                        "chunk could not be decoded during verification",
                        str(path),
                        {"index": record.index, "error": str(exc)},
                    )
                )
                continue
            if decoded != record.samples:
                quarantine_chunk(
                    out_dir,
                    record.name,
                    record.index,
                    f"decoded {decoded} samples, index says {record.samples}",
                )
                outcome.quarantined.append(
                    {
                        "index": record.index,
                        "name": record.name,
                        "decoded_samples": decoded,
                        "index_samples": record.samples,
                    }
                )
                outcome.diagnostics.append(
                    Diagnostic(
                        Code.CHUNK_HASH_MISMATCH,
                        "decoded length disagrees with the index",
                        str(path),
                        {"index": record.index, "decoded": decoded},
                    )
                )
                continue
        outcome.verified.append(record.index)
        outcome.readable_samples += record.samples
    return outcome
