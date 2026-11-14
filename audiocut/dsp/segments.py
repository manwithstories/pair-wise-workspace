"""Segment edge refinement and silence accounting.

Coarse VAD boundaries are walked outward while frames stay above the
exit threshold, so fade-in/fade-out ramps are traced back to their true
start/end; a protective pad then keeps the ramps intact. Silence
intervals are derived from the final segments (head, gaps, tail) and
adjacent ones are merged, which also absorbs gaps left by dropped
too-short speech segments.
"""
from __future__ import annotations

import numpy as np


def refine_edges(rms_db: np.ndarray, coarse: list[tuple[int, int]],
                 exit_db: float, frame_len: int, hop: int,
                 n_samples: int, pad_samples: int) -> list[tuple[int, int]]:
    """Convert coarse frame segments to padded sample-accurate bounds."""
    refined: list[list[int]] = []
    n_frames = int(rms_db.shape[0])
    for start_f, end_f in coarse:
        while start_f > 0 and rms_db[start_f - 1] >= exit_db:
            start_f -= 1
        while end_f < n_frames and rms_db[end_f] >= exit_db:
            end_f += 1
        start = max(0, start_f * hop - pad_samples)
        end = min(n_samples, end_f * hop + frame_len + pad_samples)
        refined.append([start, end])
    refined.sort(key=lambda se: se[0])
    for i in range(1, len(refined)):
        if refined[i][0] < refined[i - 1][1]:
            mid = (refined[i][0] + refined[i - 1][1]) // 2
            refined[i - 1][1] = mid
            refined[i][0] = mid
    return [(s, e) for s, e in refined if e > s]


def merge_intervals(intervals: list[tuple[int, int]]) -> list[tuple[int, int]]:
    merged: list[list[int]] = []
    for s, e in sorted(intervals):
        if merged and s <= merged[-1][1]:
            merged[-1][1] = max(merged[-1][1], e)
        else:
            merged.append([s, e])
    return [(s, e) for s, e in merged]


def silence_intervals(segments: list[tuple[int, int]],
                      n_samples: int) -> list[tuple[int, int]]:
    """Silence between (and around) segments, adjacent intervals merged."""
    out: list[tuple[int, int]] = []
    prev = 0
    for start, end in segments:
        if start > prev:
            out.append((prev, start))
        prev = max(prev, end)
    if prev < n_samples:
        out.append((prev, n_samples))
    return merge_intervals(out)


def silence_totals(intervals: list[tuple[int, int]],
                   sample_rate: int) -> tuple[int, float]:
    total = sum(e - s for s, e in intervals)
    return total, round(total / float(sample_rate), 2)
