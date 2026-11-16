"""Pure interval algebra used by the detector.

These helpers carry no audio state, which keeps the detection stages
separately testable and keeps every file inside the patch-size budget.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Sequence

import numpy as np


@dataclass(frozen=True)
class Interval:
    start: int
    end: int

    @property
    def length(self) -> int:
        return self.end - self.start


def runs_from_mask(mask: np.ndarray) -> list[Interval]:
    runs: list[Interval] = []
    start = None
    for i, flag in enumerate(mask):
        if flag and start is None:
            start = i
        elif not flag and start is not None:
            runs.append(Interval(start, i))
            start = None
    if start is not None:
        runs.append(Interval(start, mask.size))
    return runs


def drop_short_runs(runs: Sequence[Interval], min_frames: int) -> list[Interval]:
    if min_frames <= 0:
        return list(runs)
    return [r for r in runs if r.length >= min_frames]


def merge_close_runs(runs: Sequence[Interval], max_gap_frames: int) -> list[Interval]:
    """Merge runs separated by a gap shorter than the minimum silence."""

    merged: list[Interval] = []
    for run in runs:
        if merged and run.start - merged[-1].end < max_gap_frames:
            merged[-1] = Interval(merged[-1].start, run.end)
        else:
            merged.append(run)
    return merged


def ms_to_frames(ms: float, hop_ms: float) -> int:
    if ms <= 0 or hop_ms <= 0:
        return 0
    return int(math.ceil(ms / hop_ms))


def bridge_pulses(
    mask: np.ndarray, max_frames: int, min_gap_frames: int
) -> np.ndarray:
    """Absorb short gaps into their neighbouring active runs.

    A gap is bridged only when it is genuinely short -- below both
    ``max_pulse_ms`` and ``min_silence_ms``.  A gap at least as long as the
    minimum silence is a real pause and is preserved.
    """

    out = mask.copy()
    if max_frames <= 0:
        return out
    runs = runs_from_mask(mask)
    if len(runs) < 2:
        return out
    for left, right in zip(runs, runs[1:]):
        gap = right.start - left.end
        if 0 < gap <= max_frames and gap < min_gap_frames:
            out[left.end : right.start] = True
    return out


def apply_padding(
    intervals: Sequence[Interval], pad_samples: int, total_samples: int
) -> list[Interval]:
    """Grow each interval into neighbouring silence, bounded by neighbours."""

    if pad_samples <= 0 or not intervals:
        return [Interval(i.start, i.end) for i in intervals]
    padded: list[Interval] = []
    for idx, cur in enumerate(intervals):
        left_limit = intervals[idx - 1].end if idx > 0 else 0
        right_limit = (
            intervals[idx + 1].start if idx + 1 < len(intervals) else total_samples
        )
        pad_left = min(pad_samples, max(0, cur.start - left_limit))
        pad_right = min(pad_samples, max(0, right_limit - cur.end))
        padded.append(Interval(cur.start - pad_left, cur.end + pad_right))
    return padded


def complement_intervals(
    intervals: Sequence[Interval], total_samples: int
) -> list[Interval]:
    gaps: list[Interval] = []
    cursor = 0
    for item in intervals:
        if item.start > cursor:
            gaps.append(Interval(cursor, item.start))
        cursor = max(cursor, item.end)
    if cursor < total_samples:
        gaps.append(Interval(cursor, total_samples))
    return gaps
