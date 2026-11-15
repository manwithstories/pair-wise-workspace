"""Timestamp rewriting: PTS restore, DTS rebuild, ctts/elst generation.

Matroska stores presentation timestamps only.  MP4 needs strictly
increasing decode timestamps plus composition offsets.  DTS is rebuilt
from sorted PTS; negative composition offsets are shifted away and
compensated with an edit-list media time.  NumPy performs the monotonic
and frame-interval validation.
"""
from __future__ import annotations

from dataclasses import dataclass

import numpy as np

MAX_ROUND_TRIP_ERR_NS = 1_000_000  # 1 ms tolerance for timescale conversion
REORDER_WINDOW_FRAMES = 4  # B-frame reorder depth tolerated before repair


class TimestampError(Exception):
    """Timestamps cannot be repaired (maps to CLI exit code 4)."""


@dataclass
class TimingTable:
    timescale: int
    pts_ticks: np.ndarray
    dts: np.ndarray
    ctts: np.ndarray | None
    stts: list
    ctts_table: list | None
    elst_media_time: int
    duration_ticks: int
    first_pts_ticks: int
    last_pts_ticks: int
    repairs: int


def _runs(values) -> list:
    runs = []
    for value in values:
        v = int(value)
        if runs and runs[-1][1] == v:
            runs[-1][0] += 1
        else:
            runs.append([1, v])
    return [(count, v) for count, v in runs]


def build_timing(pts_ns, timescale: int) -> TimingTable:
    pts = np.asarray(list(pts_ns), dtype=np.int64)
    if pts.size == 0:
        raise TimestampError("track has no frames")
    ticks = (pts * timescale + 500_000_000) // 1_000_000_000
    back = ticks * 1_000_000_000 // timescale
    if int(np.abs(back - pts).max(initial=0)) > MAX_ROUND_TRIP_ERR_NS:
        raise TimestampError("timescale conversion exceeds 1ms error")
    ticks = ticks.copy()
    repairs = 0

    diffs = np.diff(ticks)
    positive = diffs[diffs > 0]
    interval = int(np.median(positive)) if positive.size else 1
    for i in np.nonzero(diffs == 0)[0]:  # duplicate timestamps
        ticks[i + 1] = ticks[i] + 1
        repairs += 1
    diffs = np.diff(ticks)
    limit = REORDER_WINDOW_FRAMES * interval
    for i in np.nonzero(diffs < -limit)[0]:  # severe regression
        ticks[i + 1] = ticks[i] + interval
        repairs += 1
    if repairs and repairs * 2 >= pts.size:
        raise TimestampError(
            f"timestamps unrecoverable: {repairs}/{pts.size} frames repaired"
        )

    order = np.argsort(ticks, kind="stable")
    sorted_ticks = ticks[order]
    dts = sorted_ticks - sorted_ticks[0]
    for i in range(1, dts.size):
        if dts[i] <= dts[i - 1]:
            dts[i] = dts[i - 1] + 1
            repairs += 1
    if repairs and repairs * 2 >= pts.size:
        raise TimestampError(
            f"timestamps unrecoverable: {repairs}/{pts.size} frames repaired"
        )

    ctts = None
    ctts_table = None
    elst_media_time = 0
    if not np.array_equal(dts, ticks - ticks[0]):
        ctts_vals = (ticks - sorted_ticks[0]) - dts
        min_ctts = int(ctts_vals.min())
        if min_ctts < 0:
            ctts_vals = ctts_vals - min_ctts
            elst_media_time = -min_ctts
        ctts = ctts_vals
        ctts_table = _runs(ctts_vals)

    deltas = np.diff(dts)
    sample_deltas = np.concatenate([deltas, [interval]])
    stts = _runs(sample_deltas)
    duration = int(dts[-1]) + interval
    return TimingTable(
        timescale=timescale,
        pts_ticks=ticks,
        dts=dts,
        ctts=ctts,
        stts=stts,
        ctts_table=ctts_table,
        elst_media_time=elst_media_time,
        duration_ticks=duration,
        first_pts_ticks=int(ticks.min()),
        last_pts_ticks=int(ticks.max()),
        repairs=repairs,
    )
