"""Timestamp anomaly classification and monotonic timeline rebuilding."""

from __future__ import annotations

from statistics import median
from typing import Iterable, Any

from frameaudit.model import PresentedFrame, RepairedFrame


DEFAULT_CROSS_JUMP_US = 500_000


def median_frame_interval_us(pts_values: Iterable[int | None], fallback: int = 41_667) -> int:
    values = [value for value in pts_values if value is not None]
    deltas = [right - left for left, right in zip(values, values[1:])]
    positive = [delta for delta in deltas if delta > 0]
    if not positive:
        return max(1, fallback)
    return max(1, round(median(positive)))


class TimelineRepairer:
    def __init__(
        self,
        clock_us: int,
        cross_jump_us: int = DEFAULT_CROSS_JUMP_US,
    ) -> None:
        if clock_us <= 0:
            raise ValueError("clock_us must be positive")
        if cross_jump_us <= 0:
            raise ValueError("cross_jump_us must be positive")
        self.clock_us = clock_us
        self.cross_jump_us = cross_jump_us
        self.repaired_count = 0
        self.categories = {
            "missing_pts": 0,
            "duplicate_pts": 0,
            "pts_rollback": 0,
            "gap": 0,
            "segment_jump": 0,
        }
        self.max_jump_before_us = 0
        self.max_jump_after_us = 0
        self._next_index = 0
        self._start_us: int | None = None
        self._previous_raw: int | None = None
        self._previous_segment: int | None = None
        self._previous_fixed: int | None = None
        self._monotonic = True

    def _classify(self, frame: PresentedFrame) -> str | None:
        if frame.missing_pts:
            return "missing_pts"
        if frame.decode_delta_us is not None and frame.decode_delta_us < 0:
            return "pts_rollback"
        if self._previous_raw is None:
            return None
        if self._previous_segment != frame.segment:
            return "segment_jump"
        delta = frame.pts_raw_us - self._previous_raw
        if delta == 0:
            return "duplicate_pts"
        if delta < 0:
            return "pts_rollback"
        if delta >= self.cross_jump_us:
            return "segment_jump"
        if delta > self.clock_us * 1.5:
            return "gap"
        return None

    def fix(self, frame: PresentedFrame) -> RepairedFrame:
        raw = frame.pts_raw_us
        if self._start_us is None:
            self._start_us = raw if raw is not None and raw >= 0 else 0
        fixed = self._start_us + self._next_index * self.clock_us
        category = self._classify(frame)

        if raw is not None:
            if self._previous_raw is not None:
                delta = raw - self._previous_raw
                self.max_jump_before_us = max(self.max_jump_before_us, abs(delta))
            self._previous_raw = raw
        self._previous_segment = frame.segment

        if self._previous_fixed is not None:
            delta = fixed - self._previous_fixed
            self.max_jump_after_us = max(self.max_jump_after_us, delta)
            if delta <= 0:
                self._monotonic = False
        self._previous_fixed = fixed
        self._next_index += 1

        if category is not None:
            self.repaired_count += 1
            self.categories[category] += 1

        return RepairedFrame(
            present_idx=frame.present_idx,
            decode_idx=frame.decode_idx,
            pts_raw_us=raw,
            pts_fixed_us=fixed,
            pict_type=frame.pict_type,
            keyframe=frame.keyframe,
            plane_hash=frame.plane_hash,
            category=category,
            frame=frame.frame,
        )

    def summary(self) -> dict[str, Any]:
        return {
            "repaired_count": self.repaired_count,
            "timestamp_categories": dict(self.categories),
            "max_jump_before_ms": round(self.max_jump_before_us / 1000, 3),
            "max_jump_after_ms": round(self.max_jump_after_us / 1000, 3),
            "fixed_monotonic": self._monotonic,
            "clock_ms": round(self.clock_us / 1000, 3),
        }


def repair_frames(
    frames: Iterable[PresentedFrame],
    clock_us: int,
    cross_jump_us: int = DEFAULT_CROSS_JUMP_US,
) -> tuple[list[RepairedFrame], dict[str, Any]]:
    repairer = TimelineRepairer(clock_us, cross_jump_us)
    repaired = [repairer.fix(frame) for frame in frames]
    return repaired, repairer.summary()

