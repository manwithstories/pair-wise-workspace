"""Frame-type distribution, GOP structure and CFR/VFR classification."""

from __future__ import annotations

import statistics
from fractions import Fraction

import numpy as np

from .framewalk import FrameRecord
from .repair import RepairRow


def infer_pict_type(record: FrameRecord, max_prior_pts: int | None) -> str:
    """Heuristic pict_type when the decoder did not report one.

    Keyframes are I. A non-keyframe whose PTS exceeds every previously seen
    PTS (in decode order) is a reference frame -> P; otherwise it sits
    between references -> B.
    """
    if record.keyframe:
        return "I"
    if record.pts is None:
        return "P"
    if max_prior_pts is None or record.pts > max_prior_pts:
        return "P"
    return "B"


def frame_type_counts(records: list[FrameRecord]) -> dict:
    """I/P/B distribution over decode-order records, with inference count."""
    counts = {"i_frames": 0, "p_frames": 0, "b_frames": 0, "pict_type_inferred": 0}
    max_prior_pts: int | None = None
    for record in records:
        pict = record.pict_type
        if pict not in ("I", "P", "B"):
            pict = infer_pict_type(record, max_prior_pts)
            counts["pict_type_inferred"] += 1
        counts[{  # noqa: C401
            "I": "i_frames",
            "P": "p_frames",
            "B": "b_frames",
        }[pict]] += 1
        if record.pts is not None:
            if max_prior_pts is None or record.pts > max_prior_pts:
                max_prior_pts = record.pts
    return counts


def gop_lengths(rows: list[RepairRow]) -> list[int]:
    """Distances between consecutive keyframes in display order."""
    key_positions = [r.frame_index for r in rows if r.source.keyframe]
    if not key_positions:
        return []
    gops = [b - a for a, b in zip(key_positions, key_positions[1:])]
    tail = len(rows) - key_positions[-1]
    if tail:
        gops.append(tail)
    return gops


def rate_analysis(rows: list[RepairRow], time_base: Fraction) -> dict:
    """Fit the nominal frame rate from the median PTS delta; classify CFR/VFR."""
    pts = [r.pts for r in rows if r.pts is not None]
    deltas = [b - a for a, b in zip(pts, pts[1:]) if b - a > 0]
    result = {
        "nominal_fps": None,
        "vfr": False,
        "vfr_score": 0.0,
        "confidence": 0.0,
        "median_interval_ticks": None,
    }
    if not deltas:
        return result
    median = statistics.median(deltas)
    result["median_interval_ticks"] = median
    fps = 1.0 / (median * float(time_base)) if median > 0 else None
    if fps is None or not (0.1 <= fps <= 1000.0):
        return result
    deviations = np.abs(np.asarray(deltas, dtype=float) - median) / median
    p95 = float(np.percentile(deviations, 95))
    result.update(
        nominal_fps=round(fps, 3),
        vfr=bool(p95 > 0.05),
        vfr_score=round(min(1.0, p95 / 0.25), 4),
        confidence=round(min(1.0, len(deltas) / 120.0), 3),
    )
    return result
