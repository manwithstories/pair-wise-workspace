"""Offscreen frame export and per-frame quality/index records.

No GUI or browser is involved.  Frames are written atomically (temp file
+ ``os.replace``) so a write interruption can never leave a file that
looks complete.  Every frame gets an exact media timestamp, its source
segment and a SHA-256 hash of the raw RGB pixels; the number of hashes
always equals the number of exported frames.
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import numpy as np

from .compose import TimelineFrame
from .errors import ErrorCode, FatalError


@dataclass(slots=True)
class FrameRecord:
    index: int
    pts_seconds: float
    segment: str
    sequence: int
    filled: bool
    pixel_hash: str
    file_name: str

    def to_dict(self) -> dict:
        return {
            "index": self.index,
            "pts_seconds": self.pts_seconds,
            "segment": self.segment,
            "sequence": self.sequence,
            "filled": self.filled,
            "pixel_hash": self.pixel_hash,
            "file": self.file_name,
        }


@dataclass(slots=True)
class FrameExporter:
    out_dir: Path
    max_frames: int
    records: list[FrameRecord] = field(default_factory=list)
    frames_dir: Path = field(init=False)

    def __post_init__(self) -> None:
        self.frames_dir = self.out_dir / "frames"
        self.frames_dir.mkdir(parents=True, exist_ok=True)

    def _atomic_write(self, dest: Path, rgb: np.ndarray) -> None:
        fd, tmp_name = tempfile.mkstemp(prefix=".frame-", suffix=".raw",
                                        dir=str(self.frames_dir))
        try:
            with os.fdopen(fd, "wb") as fh:
                fh.write(np.ascontiguousarray(rgb).tobytes())
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp_name, dest)
        except OSError as exc:
            if os.path.exists(tmp_name):
                os.unlink(tmp_name)
            raise FatalError(
                ErrorCode.WRITE_INTERRUPTED,
                f"failed writing frame {dest.name}: {exc}",
            ) from exc

    def export(self, frame: TimelineFrame) -> FrameRecord:
        if len(self.records) >= self.max_frames:
            raise FatalError(
                ErrorCode.LIMIT_EXCEEDED,
                f"output frame limit {self.max_frames} reached",
            )
        index = len(self.records)
        digest = hashlib.sha256(
            np.ascontiguousarray(frame.rgb).tobytes()
        ).hexdigest()
        frame.pixel_hash = digest
        name = f"frame-{index:06d}.raw"
        self._atomic_write(self.frames_dir / name, frame.rgb)
        record = FrameRecord(
            index=index,
            pts_seconds=float(frame.pts_seconds),
            segment=frame.segment,
            sequence=frame.sequence,
            filled=frame.filled,
            pixel_hash=digest,
            file_name=name,
        )
        self.records.append(record)
        return record

    def write_index(self) -> Path:
        path = self.out_dir / "frame_index.json"
        payload = json.dumps([r.to_dict() for r in self.records], indent=2)
        tmp = path.with_suffix(".json.tmp")
        tmp.write_text(payload, encoding="utf-8")
        os.replace(tmp, path)
        return path

    @property
    def hashes(self) -> list[str]:
        return [r.pixel_hash for r in self.records]


def frame_psnr(reference: np.ndarray, target: np.ndarray) -> float:
    """PSNR in dB between two uint8 RGB frames; capped at 60 dB.

    A small epsilon floor keeps the value finite for identical frames,
    while gap-filled blank frames yield a finite, deliberately low score.
    """
    if reference.shape != target.shape:
        target = np.resize(target, reference.shape)
    diff = (reference.astype(np.float64) - target.astype(np.float64))
    mse = float(np.mean(diff * diff))
    if mse <= 1e-10:
        return 60.0
    return min(60.0, float(10.0 * np.log10((255.0 ** 2) / mse)))


def manifest_alignment_ms(
    declared_durations: list[Fraction],
    segment_start_pts: list[Fraction],
) -> float:
    """Mean segment-boundary timing error in milliseconds.

    The manifest declares each segment's duration; the reconstructed
    timeline records each segment's first presentation timestamp.  The
    boundary error is the difference between consecutive observed segment
    starts and the declared duration of the preceding segment.  Measuring
    at boundaries (not the last frame) keeps intra-segment VFR jitter out
    of the alignment figure, while still catching real drift or gaps.
    """
    if len(segment_start_pts) < 2:
        return 0.0
    errors = []
    for i in range(1, len(segment_start_pts)):
        observed_step = segment_start_pts[i] - segment_start_pts[i - 1]
        errors.append(abs(float(observed_step - declared_durations[i - 1])))
    return float(sum(errors) / len(errors)) * 1000.0
