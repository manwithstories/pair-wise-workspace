"""Numeric helpers shared by the timestamp planner."""

from __future__ import annotations

import numpy as np

from ._timing import TimingConfig, TimestampError
from .matroska_model import NANOS_PER_SECOND


def ns_to_ticks(ns: np.ndarray, timescale: int) -> np.ndarray:
    return np.rint(np.asarray(ns, dtype=np.float64) * timescale / NANOS_PER_SECOND).astype(np.int64)


def nudge_duplicates(pts: np.ndarray, step: int, config: TimingConfig) -> np.ndarray:
    """Push repeated PTS values forward by the minimum legal step.

    A run of identical values becomes ``v, v+step, v+2*step``: each duplicate is
    detected against the original predecessor and placed past the value already
    written. A PTS lower than its predecessor is presentation reordering
    (B-frames) and is left for the composition table.
    """
    out = pts.copy()
    for i in range(1, out.size):
        if pts[i] != pts[i - 1]:
            continue
        # Compare against the *original* predecessor: a run of three equal
        # values is two duplicates, even though the middle one has already
        # been moved by the time the third is examined.
        if not config.repair_duplicates:
            raise TimestampError("duplicate_timestamps", f"duplicate PTS at {i}")
        out[i] = max(pts[i], out[i - 1] + step)
    return out


def force_monotonic(pts: np.ndarray, step: int) -> np.ndarray:
    """Flatten all regressions into a strictly increasing presentation order."""
    out = pts.copy()
    for i in range(1, out.size):
        if out[i] <= out[i - 1]:
            out[i] = out[i - 1] + step
    return out


def grid_dts(pts: np.ndarray, step: int) -> np.ndarray:
    """Lay decode times on a uniform grid spanning the presentation window."""
    span = int(pts[-1] - pts[0])
    grid_step = max(step, int(round(span / max(1, pts.size - 1))))
    return pts[0] + np.arange(pts.size, dtype=np.int64) * grid_step


def ticks_to_target(ticks: int, source_scale_ns: int, timescale: int) -> int:
    return int(round(ticks * source_scale_ns * timescale / NANOS_PER_SECOND))

__all__ = [
    "force_monotonic",
    "grid_dts",
    "ns_to_ticks",
    "nudge_duplicates",
    "ticks_to_target",
]
