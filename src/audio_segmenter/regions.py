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
    """Grow each interval into neighbouring silence, bounded by neighbours.

    A gap between two intervals is shared: each side may take at most half of
    it, so padding can never make neighbours overlap.  File edges may use the
    full padding budget up to the file boundary.
    """

    if pad_samples <= 0 or not intervals:
        return [Interval(i.start, i.end) for i in intervals]
    padded: list[Interval] = []
    for idx, cur in enumerate(intervals):
        if idx > 0:
            gap_left = max(0, cur.start - intervals[idx - 1].end)
            left_budget = min(pad_samples, gap_left // 2)
            left_budget = min(left_budget, cur.start)
        else:
            left_budget = min(pad_samples, cur.start)
        if idx + 1 < len(intervals):
            gap_right = max(0, intervals[idx + 1].start - cur.end)
            right_budget = min(pad_samples, gap_right // 2)
        else:
            right_budget = min(pad_samples, max(0, total_samples - cur.end))
        padded.append(Interval(cur.start - left_budget, cur.end + right_budget))
    return padded


def merge_close_intervals(
    intervals: Sequence[Interval], max_gap_samples: int
) -> list[Interval]:
    """Merge intervals whose separating gap is shorter than the min silence.

    A gap at least as long as ``min_silence_ms`` is a real pause and is kept.
    Working in samples (not frames) keeps the boundary exact regardless of
    hop size, so a configured 120 ms threshold means 120 ms.
    """

    merged: list[Interval] = []
    for run in intervals:
        if merged and run.start - merged[-1].end < max_gap_samples:
            merged[-1] = Interval(merged[-1].start, run.end)
        else:
            merged.append(run)
    return merged


def drop_short_intervals(
    intervals: Sequence[Interval], min_length_samples: int
) -> list[Interval]:
    if min_length_samples <= 0:
        return list(intervals)
    return [i for i in intervals if i.length >= min_length_samples]


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
