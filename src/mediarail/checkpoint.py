"""Checkpoint persistence: input fingerprints, stage cursors, temp outputs.

A checkpoint is written at packet boundaries so an interrupted run can be
resumed.  On resume the input is re-fingerprinted (size, mtime, chunked
SHA-256); any change rejects the resume, and the temporary output is
truncated back to the last committed byte boundary before continuing.
"""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import asdict, dataclass, field
from pathlib import Path

from .errors import CheckpointError, ErrorCode

CHUNK_SIZE = 4 * 1024 * 1024
CHECKPOINT_VERSION = 1


@dataclass
class InputFingerprint:
    size: int
    mtime_ns: int
    chunk_sha256: list[str] = field(default_factory=list)

    @property
    def digest(self) -> str:
        h = hashlib.sha256()
        for chunk in self.chunk_sha256:
            h.update(bytes.fromhex(chunk))
        return h.hexdigest()


def compute_fingerprint(path: str | Path, chunk_size: int = CHUNK_SIZE) -> InputFingerprint:
    """Fingerprint an input by size, mtime and per-chunk SHA-256."""
    path = Path(path)
    stat = path.stat()
    chunks: list[str] = []
    with path.open("rb") as fh:
        while True:
            data = fh.read(chunk_size)
            if not data:
                break
            chunks.append(hashlib.sha256(data).hexdigest())
    return InputFingerprint(
        size=stat.st_size, mtime_ns=stat.st_mtime_ns, chunk_sha256=chunks
    )


def verify_fingerprint(path: str | Path, expected: InputFingerprint) -> None:
    """Raise :class:`CheckpointError` if the input no longer matches."""
    current = compute_fingerprint(path)
    if current.size != expected.size or current.mtime_ns != expected.mtime_ns:
        raise CheckpointError(
            f"input {path} changed (size/mtime mismatch)",
            code=ErrorCode.INPUT_CHANGED,
        )
    if current.chunk_sha256 != expected.chunk_sha256:
        raise CheckpointError(
            f"input {path} changed (chunk hash mismatch)",
            code=ErrorCode.INPUT_CHANGED,
        )


@dataclass
class Checkpoint:
    """Durable state of one input file's pipeline run."""

    input_path: str
    output_path: str
    temp_output_path: str
    fingerprint: InputFingerprint
    stage: str = "scan"                # scan -> mux -> index -> done
    committed_packets: int = 0         # packets safely in the temp output
    committed_bytes: int = 0           # committed length of the temp output
    output_sha256: str | None = None

    def to_dict(self) -> dict:
        data = asdict(self)
        data["version"] = CHECKPOINT_VERSION
        return data

    @classmethod
    def from_dict(cls, data: dict) -> "Checkpoint":
        if data.get("version") != CHECKPOINT_VERSION:
            raise CheckpointError("unsupported checkpoint version")
        return cls(
            input_path=data["input_path"],
            output_path=data["output_path"],
            temp_output_path=data["temp_output_path"],
            fingerprint=InputFingerprint(**data["fingerprint"]),
            stage=data["stage"],
            committed_packets=data["committed_packets"],
            committed_bytes=data["committed_bytes"],
            output_sha256=data.get("output_sha256"),
        )


class CheckpointStore:
    """One JSON checkpoint file per input, inside a state directory."""

    def __init__(self, directory: str | Path):
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)

    def _file(self, key: str) -> Path:
        safe = key.replace("/", "_").replace("\\", "_")
        return self.directory / f"{safe}.checkpoint.json"

    def load(self, key: str) -> Checkpoint | None:
        path = self._file(key)
        if not path.exists():
            return None
        try:
            return Checkpoint.from_dict(json.loads(path.read_text()))
        except (json.JSONDecodeError, KeyError, TypeError) as exc:
            raise CheckpointError(f"corrupt checkpoint {path}: {exc}") from exc

    def save(self, key: str, checkpoint: Checkpoint) -> None:
        path = self._file(key)
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps(checkpoint.to_dict(), indent=1))
        os.replace(tmp, path)  # atomic commit at a packet boundary

    def remove(self, key: str) -> None:
        self._file(key).unlink(missing_ok=True)


def truncate_to(path: str | Path, size: int) -> None:
    """Truncate a temporary output back to its last committed boundary."""
    path = Path(path)
    if not path.exists():
        return
    current = path.stat().st_size
    if current > size:
        with path.open("r+b") as fh:
            fh.truncate(size)


def sha256_file(path: str | Path, chunk_size: int = CHUNK_SIZE) -> str:
    h = hashlib.sha256()
    with Path(path).open("rb") as fh:
        while True:
            data = fh.read(chunk_size)
            if not data:
                break
            h.update(data)
    return h.hexdigest()
