"""Atomic, fingerprint-checked processing checkpoints.

A checkpoint records everything required to resume a stream scan without
reprocessing or silently analysing a different input:

* the SHA-256 fingerprint and size of the input stream,
* the canonical parameter set the run was started with,
* the next source byte offset to read and packets already consumed,
* accumulated per-track state and the serialised damage findings.

Writes are atomic (temp file + ``os.replace``) and fsync'd, so a crash never
leaves a half-written checkpoint.  On resume, any fingerprint/parameter/offset
inconsistency raises :class:`StateMismatch` which the CLI maps to exit code 4.
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import asdict, dataclass, field

CHECKPOINT_VERSION = 1
#: How many input bytes are hashed per chunk (kept small for the 64 MiB cap).
_HASH_CHUNK = 1 << 20


class StateMismatch(RuntimeError):
    """Raised when a checkpoint cannot safely resume the requested run."""


@dataclass(slots=True)
class CheckpointState:
    version: int = CHECKPOINT_VERSION
    input_path: str = ""
    input_sha256: str = ""
    input_size: int = 0
    params: dict = field(default_factory=dict)
    next_offset: int = 0
    packets_processed: int = 0
    track_state: dict = field(default_factory=dict)
    damage: list[dict] = field(default_factory=list)
    wraps: dict = field(default_factory=dict)
    collected_count: int = 0

    def to_json(self) -> str:
        return json.dumps(asdict(self), sort_keys=True, separators=(",", ":"))

    @classmethod
    def from_json(cls, raw: str) -> "CheckpointState":
        data = json.loads(raw)
        if data.get("version") != CHECKPOINT_VERSION:
            raise StateMismatch(
                f"checkpoint version {data.get('version')} != {CHECKPOINT_VERSION}")
        fields = {f for f in cls.__slots__}
        unknown = set(data) - fields
        if unknown:
            raise StateMismatch(f"unknown checkpoint fields: {sorted(unknown)}")
        return cls(**data)


def fingerprint_file(path: str) -> tuple[str, int]:
    """Return ``(sha256, size)`` streaming the file in fixed-size chunks."""
    digest = hashlib.sha256()
    size = 0
    with open(path, "rb") as fh:
        while True:
            chunk = fh.read(_HASH_CHUNK)
            if not chunk:
                break
            digest.update(chunk)
            size += len(chunk)
    return digest.hexdigest(), size


def save_checkpoint(path: str, state: CheckpointState) -> None:
    """Atomically write ``state`` to ``path`` (same directory temp + replace)."""
    directory = os.path.dirname(os.path.abspath(path)) or "."
    fd, tmp = tempfile.mkstemp(prefix=".relaydiag-ckpt-", dir=directory)
    try:
        with os.fdopen(fd, "w") as fh:
            fh.write(state.to_json())
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except BaseException:
        if os.path.exists(tmp):
            os.unlink(tmp)
        raise


def load_checkpoint(path: str) -> CheckpointState:
    with open(path, "r", encoding="utf-8") as fh:
        return CheckpointState.from_json(fh.read())


def validate_for_resume(saved: CheckpointState, *, input_path: str,
                        params: dict) -> None:
    """Raise :class:`StateMismatch` unless the run can safely continue.

    The on-disk input is re-fingerprinted and compared against the checkpoint;
    parameters must match canonically, and the saved offset must lie inside
    the current file.
    """
    problems: list[str] = []
    digest, size = fingerprint_file(input_path)
    if saved.input_sha256 != digest:
        problems.append(
            f"input fingerprint mismatch: checkpoint {saved.input_sha256[:12]} "
            f"!= current {digest[:12]}")
    if saved.input_size != size:
        problems.append(
            f"input size mismatch: checkpoint {saved.input_size} != current {size}")
    if saved.params != params:
        problems.append("run parameters differ from those stored in checkpoint")
    if not (0 <= saved.next_offset <= size):
        problems.append(
            f"saved offset {saved.next_offset} outside input of {size} bytes")
    if problems:
        raise StateMismatch("; ".join(problems))
