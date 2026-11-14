"""NumPy-vectorized statistics aggregated from a FrameTimeline."""

from __future__ import annotations

import numpy as np

from .timeline import PICT_B, PICT_I, PICT_P, FrameTimeline


def _max_jump_units(timeline: FrameTimeline) -> float:
    """Largest positive excess of a consecutive pts delta over one interval."""
    valid = ~np.isnan(timeline.pts_original)
    pts = timeline.pts_original[valid]
    if pts.shape[0] < 2:
        return 0.0
    excess = np.diff(pts) - timeline.interval
    return max(0.0, float(excess.max()))


def jump_quantiles(timeline: FrameTimeline) -> dict:
    """Quantiles of positive consecutive-delta excess, in milliseconds."""
    valid = ~np.isnan(timeline.pts_original)
    pts = timeline.pts_original[valid]
    if pts.shape[0] < 2:
        return {"p50": 0.0, "p95": 0.0}
    excess = np.diff(pts) - timeline.interval
    excess = np.clip(excess, 0.0, None)
    q50, q95 = np.percentile(excess, [50.0, 95.0])
    return {
        "p50": round(timeline.to_ms(q50), 3),
        "p95": round(timeline.to_ms(q95), 3),
    }


def summarize(timeline: FrameTimeline) -> dict:
    """Aggregate the report fields from the timeline (single source)."""
    counts = np.bincount(timeline.pict_type, minlength=4)
    max_jump_ms = round(timeline.to_ms(_max_jump_units(timeline)), 3)
    if max_jump_ms == int(max_jump_ms):
        max_jump_ms = int(max_jump_ms)
    return {
        "frames_decoded": int(timeline.frames_decoded),
        "i_count": int(counts[PICT_I]),
        "p_count": int(counts[PICT_P]),
        "b_count": int(counts[PICT_B]),
        "ts_repaired": int(timeline.repair_mask.sum()),
        "max_jump_ms": max_jump_ms,
        "duration_ms": int(round(timeline.to_ms(timeline.duration_units))),
        "warnings": list(timeline.warnings),
    }
