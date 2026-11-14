"""Timestamp repair: rebuild a monotonic timeline on the nominal frame grid.

Anomalies: duplicates / regressions (pts below expected - tolerance), holes
and jumps beyond 3x the nominal interval (pts above expected + tolerance),
and missing timestamps. Every rebuilt frame is counted in ``ts_repaired``.
"""

from __future__ import annotations

from fractions import Fraction

import numpy as np

from .decoder import DecodeResult
from .probe import ProbeInfo
from .timeline import FrameTimeline, pict_code

WARN_REGRESSION = "timestamp_regression"
WARN_GAP = "gap"
WARN_MISSING = "timestamps_missing"
WARN_TRUNCATED = "truncated_tail"


def nominal_interval(info: ProbeInfo) -> int:
    """Nominal pts step in stream time_base units."""
    fps = info.avg_frame_rate
    if fps <= 0:
        fps = Fraction(25, 1)
    step = 1.0 / (float(fps) * float(info.time_base))
    return max(1, int(round(step)))


def dominant_interval(pts_orig: np.ndarray, fallback: int) -> int:
    """Effective nominal interval from the data itself.

    Corrupted timestamps pollute the container's average frame rate, so the
    median positive consecutive delta is a more reliable nominal step; the
    probe-based value is the fallback when deltas are unavailable.
    """
    valid = pts_orig[~np.isnan(pts_orig)]
    if valid.shape[0] >= 2:
        positive = np.diff(valid)
        positive = positive[positive > 0]
        if positive.size:
            return max(1, int(round(float(np.median(positive)))))
    return fallback


def _add_warning(warnings: list, name: str) -> None:
    if name not in warnings:
        warnings.append(name)


def _repair_pts(pts_orig: np.ndarray, interval: int, warnings: list):
    n = pts_orig.shape[0]
    repaired = np.zeros(n, dtype=np.int64)
    mask = np.zeros(n, dtype=bool)
    valid = ~np.isnan(pts_orig)

    if n == 0:
        return repaired, mask
    if not valid.any():
        # All timestamps missing: rebuild the whole run, count every frame.
        repaired[:] = np.arange(n, dtype=np.int64) * interval
        mask[:] = True
        _add_warning(warnings, WARN_MISSING)
        return repaired, mask

    tol = interval / 2.0
    if valid[0]:
        repaired[0] = int(round(pts_orig[0]))
    else:
        repaired[0] = 0
        mask[0] = True
        _add_warning(warnings, WARN_MISSING)

    for i in range(1, n):
        expected = repaired[i - 1] + interval
        if not valid[i]:
            repaired[i] = expected
            mask[i] = True
            _add_warning(warnings, WARN_MISSING)
            continue
        pts = pts_orig[i]
        if pts < expected - tol:
            # Duplicate (pts == previous) or backward jump.
            repaired[i] = expected
            mask[i] = True
            _add_warning(warnings, WARN_REGRESSION)
        elif pts > expected + tol:
            # Hole or single-frame jump beyond 3x the nominal interval.
            repaired[i] = expected
            mask[i] = True
            _add_warning(warnings, WARN_GAP)
        else:
            repaired[i] = int(round(pts))
    return repaired, mask


def build_timeline(
    decode: DecodeResult,
    ordered,
    info: ProbeInfo,
    reorder_warnings,
) -> FrameTimeline:
    """Combine reordered records and repaired timestamps into a FrameTimeline."""
    n = len(ordered)

    decode_index = np.array([r.decode_index for r in ordered], dtype=np.int64)
    pts_orig = np.array(
        [r.pts if r.pts is not None else np.nan for r in ordered], dtype=np.float64
    )
    dts_orig = np.array(
        [r.dts if r.dts is not None else np.nan for r in ordered], dtype=np.float64
    )
    pict = np.array([pict_code(r.pict_type) for r in ordered], dtype=np.uint8)

    interval = dominant_interval(pts_orig, nominal_interval(info))
    warnings = list(reorder_warnings)
    pts_repaired, repair_mask = _repair_pts(pts_orig, interval, warnings)
    if decode.truncated:
        _add_warning(warnings, WARN_TRUNCATED)

    return FrameTimeline(
        time_base=info.time_base,
        frame_rate=info.avg_frame_rate,
        interval=interval,
        decode_index=decode_index,
        pts_original=pts_orig,
        dts_original=dts_orig,
        pts_repaired=pts_repaired,
        pict_type=pict,
        repair_mask=repair_mask,
        warnings=warnings,
        frames_decoded=decode.frames_decoded,
    )
