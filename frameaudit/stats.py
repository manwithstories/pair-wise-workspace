"""Streaming frame-type, timing, and pixel-plane statistics."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from typing import Any

import av
import numpy as np


def pict_type(frame: av.VideoFrame) -> str:
    value = frame.pict_type
    name = getattr(value, "name", None)
    text = name if isinstance(name, str) else str(value)
    return text.split(".")[-1].upper()


def plane_hash(frame: av.VideoFrame) -> str | None:
    try:
        converted = frame.reformat(format="yuv420p")
        plane = converted.planes[0]
        buffer = np.frombuffer(memoryview(plane), dtype=np.uint8)
        pixels = buffer.reshape(converted.height, plane.line_size)
        pixels = pixels[:, : converted.width]
        digest = hashlib.blake2b(pixels.tobytes(order="C"), digest_size=16).hexdigest()
        return digest
    except Exception:
        return None


@dataclass(slots=True)
class FrameStats:
    frames: int = 0
    i_frames: int = 0
    p_frames: int = 0
    b_frames: int = 0
    other_frames: int = 0
    keyframe_count: int = 0
    _previous_pts: int | None = field(default=None, init=False)
    _interval_sum: int = field(default=0, init=False)
    _interval_count: int = field(default=0, init=False)
    _interval_peak: int = field(default=0, init=False)
    _previous_keyframe_pts: int | None = field(default=None, init=False)
    _keyframe_interval_sum: int = field(default=0, init=False)
    _keyframe_interval_count: int = field(default=0, init=False)
    _keyframe_interval_peak: int = field(default=0, init=False)

    def add(
        self,
        pts_us: int | None,
        frame_type: str,
        keyframe: bool,
    ) -> None:
        self.frames += 1
        if frame_type == "I":
            self.i_frames += 1
        elif frame_type == "P":
            self.p_frames += 1
        elif frame_type == "B":
            self.b_frames += 1
        else:
            self.other_frames += 1

        if keyframe:
            self.keyframe_count += 1
            if pts_us is not None and self._previous_keyframe_pts is not None:
                delta = pts_us - self._previous_keyframe_pts
                if delta >= 0:
                    self._keyframe_interval_sum += delta
                    self._keyframe_interval_count += 1
                    self._keyframe_interval_peak = max(self._keyframe_interval_peak, delta)
            if pts_us is not None:
                self._previous_keyframe_pts = pts_us

        if pts_us is not None:
            if self._previous_pts is not None:
                delta = pts_us - self._previous_pts
                if delta >= 0:
                    self._interval_sum += delta
                    self._interval_count += 1
                    self._interval_peak = max(self._interval_peak, delta)
            self._previous_pts = pts_us

    @staticmethod
    def _ms(value: int, count: int) -> float:
        return round(value / count / 1000, 3) if count else 0.0

    def as_dict(self) -> dict[str, Any]:
        return {
            "decoded_frames": self.frames,
            "i_frames": self.i_frames,
            "p_frames": self.p_frames,
            "b_frames": self.b_frames,
            "other_frames": self.other_frames,
            "keyframe_count": self.keyframe_count,
            "frame_interval_mean_ms": self._ms(self._interval_sum, self._interval_count),
            "frame_interval_peak_ms": round(self._interval_peak / 1000, 3),
            "keyframe_interval_mean_ms": self._ms(
                self._keyframe_interval_sum, self._keyframe_interval_count
            ),
            "keyframe_interval_peak_ms": round(self._keyframe_interval_peak / 1000, 3),
        }

