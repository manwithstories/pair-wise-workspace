"""Persistent batch cursor, input fingerprint and commit boundary.

A checkpoint answers one question on resume: *can I trust the bytes I already
wrote?*  To answer it we persist:

``source``
    size, mtime, path and a head/tail digest of the input.  If any of them
    changed the source is not the file we started on and resume is refused
    with exit code 3.
``profile`` / ``tool_version`` / ``index_version``
    the repair rules and software that produced the committed chunks.
    Different rules would make the resumed tail inconsistent with the head,
    so a mismatch is also refused.
``cursor``
    the last committed chunk, the global packet index the next chunk starts
    at, the last acknowledged packet, and the encoder/clock state needed to
    continue the timeline exactly where it stopped.
``output``
    the repaired file path plus its committed byte length.  On resume the
    file is truncated back to this length before appending, which is what
    makes a crash between "wrote bytes" and "committed chunk" harmless.

Writes are atomic: the new checkpoint is written to a sibling temp file,
flushed and ``os.replace``-d over the old one.  A process killed mid-write
leaves the previous checkpoint intact.
"""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import dataclass, field
from pathlib import Path

from .errors import CheckpointMismatchError, InputChangedError
from .timeline import TimelineProfile

CHECKPOINT_VERSION = 1

#: Fields that must match exactly for a resume to be accepted.
_FINGERPRINT_FIELDS = ("path", "size", "mtime_ns", "digest")


@dataclass
class Checkpoint:
    version: int = CHECKPOINT_VERSION
    tool_version: str = ""
    source: dict = field(default_factory=dict)
    profile: dict = field(default_factory=dict)
    output: dict = field(default_factory=dict)
    cursor: dict = field(default_factory=dict)
    clocks: dict = field(default_factory=dict)
    damage_count: int = 0
    packets_written: int = 0
    state: str = "running"

    # -- serialisation ----------------------------------------------------
    def as_dict(self) -> dict:
        return {
            "version": self.version,
            "tool_version": self.tool_version,
            "source": self.source,
            "profile": self.profile,
            "output": self.output,
            "cursor": self.cursor,
            "clocks": self.clocks,
            "damage_count": self.damage_count,
            "packets_written": self.packets_written,
            "state": self.state,
        }

    @classmethod
    def from_dict(cls, payload: dict) -> "Checkpoint":
        return cls(
            version=int(payload.get("version", 0)),
            tool_version=payload.get("tool_version", ""),
            source=payload.get("source", {}) or {},
            profile=payload.get("profile", {}) or {},
            output=payload.get("output", {}) or {},
            cursor=payload.get("cursor", {}) or {},
            clocks=payload.get("clocks", {}) or {},
            damage_count=int(payload.get("damage_count", 0)),
            packets_written=int(payload.get("packets_written", 0)),
            state=payload.get("state", "running"),
        )


def load_checkpoint(path: Path) -> Checkpoint:
    path = Path(path)
    if not path.exists():
        raise CheckpointMismatchError(f"checkpoint not found: {path}", context={"path": str(path)})
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise CheckpointMismatchError(f"checkpoint is unreadable or corrupt: {exc}",
                                      context={"path": str(path)}) from exc
    checkpoint = Checkpoint.from_dict(payload)
    if checkpoint.version != CHECKPOINT_VERSION:
        raise CheckpointMismatchError(
            f"checkpoint version {checkpoint.version} is not supported "
            f"(expected {CHECKPOINT_VERSION})",
            context={"path": str(path), "version": checkpoint.version},
        )
    if checkpoint.cursor.get("completed") is True:
        raise CheckpointMismatchError(
            "checkpoint belongs to an already completed run; start a fresh repair",
            context={"path": str(path)},
        )
    return checkpoint


def save_checkpoint(path: Path, checkpoint: Checkpoint) -> None:
    """Atomically replace the checkpoint; never leaves a partial file."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(checkpoint.as_dict(), indent=2, sort_keys=True)
    handle, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=str(path.parent))
    try:
        with os.fdopen(handle, "w", encoding="utf-8") as stream:
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp_name, path)
    except Exception:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise


def validate_resume(checkpoint: Checkpoint, *, fingerprint: dict, profile: TimelineProfile,
                    tool_version: str, index_version: int,
                    index_meta: dict | None = None) -> None:
    """Refuse a resume whose inputs no longer line up with the checkpoint."""
    if checkpoint.source:
        for key in _FINGERPRINT_FIELDS:
            if checkpoint.source.get(key) != fingerprint.get(key):
                raise InputChangedError(
                    f"source {key} changed since the checkpoint was written",
                    context={
                        "field": key,
                        "checkpoint": checkpoint.source.get(key),
                        "current": fingerprint.get(key),
                        "path": fingerprint.get("path"),
                    },
                )
    if checkpoint.tool_version and checkpoint.tool_version != tool_version:
        raise CheckpointMismatchError(
            f"checkpoint was written by mediabridge {checkpoint.tool_version}, "
            f"current is {tool_version}",
            context={"checkpoint_tool_version": checkpoint.tool_version,
                     "current_tool_version": tool_version},
        )
    if checkpoint.profile and checkpoint.profile != profile.as_dict():
        raise CheckpointMismatchError(
            "checkpoint repair profile differs from the requested one",
            context={"checkpoint_profile": checkpoint.profile,
                     "requested_profile": profile.as_dict()},
        )
    if index_meta is not None:
        version = int(index_meta.get("index_version", 0))
        if version != index_version:
            raise CheckpointMismatchError(
                f"index version {version} is incompatible with {index_version}",
                context={"index_version": version},
            )


def output_matches(checkpoint: Checkpoint, output_path: Path) -> int:
    """Verify the partial output is intact and return its committed length."""
    expected = int(checkpoint.output.get("committed_bytes", 0))
    path = Path(output_path)
    if expected and not path.exists():
        raise CheckpointMismatchError(
            "checkpoint references a repaired output that no longer exists",
            context={"output": str(path), "committed_bytes": expected},
        )
    if path.exists():
        actual = path.stat().st_size
        if actual < expected:
            raise CheckpointMismatchError(
                "repaired output is shorter than the last committed boundary",
                context={"output": str(path), "committed_bytes": expected, "actual_bytes": actual},
            )
    return expected


def truncate_output(output_path: Path, committed_bytes: int) -> None:
    """Drop uncommitted trailing bytes from an interrupted run."""
    path = Path(output_path)
    if not path.exists():
        return
    size = path.stat().st_size
    if size == committed_bytes:
        return
    if size < committed_bytes:
        raise CheckpointMismatchError(
            "cannot truncate output below the committed length",
            context={"output": str(path), "committed_bytes": committed_bytes, "actual": size},
        )
    with open(path, "r+b") as handle:
        handle.truncate(committed_bytes)
        handle.flush()
        os.fsync(handle.fileno())
