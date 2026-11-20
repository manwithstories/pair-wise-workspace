"""Atomic checkpoint state and restart safety.

The checkpoint records the manifest/input digest, clock wrap state and the
number of fully persisted frames.  On restart the input digest is verified
before any work resumes, and orphan frame files past the checkpoint
boundary are removed so an interrupted write can never be mistaken for a
complete frame.  All checkpoint writes are atomic (temp file + replace).
"""

from __future__ import annotations

import json
import os
import resource
import sys
from pathlib import Path

from .errors import ErrorCode, RecoverableError
from .manifest import ManifestPlan

CHECKPOINT_NAME = "checkpoint.json"


class Checkpoint:
    """Atomic JSON checkpoint with input digest verification."""

    def __init__(self, out_dir: Path) -> None:
        self.path = out_dir / CHECKPOINT_NAME
        self.data: dict = {}

    def load(self, plan: ManifestPlan) -> bool:
        if not self.path.exists():
            return False
        data = json.loads(self.path.read_text(encoding="utf-8"))
        if data.get("input_digest") != plan.input_digest:
            raise RecoverableError(
                ErrorCode.CHECKPOINT_MISMATCH,
                "input manifest/segments changed since checkpoint; restart "
                "would not correspond to the original manifest",
            )
        self.data = data
        return True

    def save(self, plan: ManifestPlan, done: list[int], wraps: int,
             frames: int) -> None:
        payload = {
            "input_digest": plan.input_digest,
            "manifest_digest": plan.manifest_digest,
            "segments_done": done,
            "wrap_count": wraps,
            "frame_count": frames,
        }
        tmp = self.path.with_suffix(".json.tmp")
        tmp.write_text(json.dumps(payload, indent=2), encoding="utf-8")
        os.replace(tmp, self.path)

    @property
    def frame_count(self) -> int:
        return int(self.data.get("frame_count", 0))


def peak_memory_mb() -> float:
    """Peak resident set size in MB.

    Uses ``ru_maxrss`` rather than tracemalloc because PyAV/FFmpeg decode
    frames through native allocations that Python's tracer never sees.
    macOS reports bytes; Linux reports kilobytes.
    """
    ru = resource.getrusage(resource.RUSAGE_SELF)
    if sys.platform == "darwin":
        return round(ru.ru_maxrss / (1024 * 1024), 2)
    return round(ru.ru_maxrss / 1024, 2)


def cleanup_orphan_frames(out_dir: Path, kept: int) -> None:
    """Delete frame files at or beyond the checkpoint's persisted count."""
    frames_dir = out_dir / "frames"
    if not frames_dir.exists():
        return
    for raw in frames_dir.glob("frame-*.raw"):
        idx = int(raw.stem.split("-")[1])
        if idx >= kept:
            raw.unlink()
