"""Numerical quality metrics for the stabilization pipeline."""

from __future__ import annotations

import numpy as np

from .motion import pose_params
from .smoother import detrend


def jitter_px(poses: list[np.ndarray]) -> float:
    """RMS high-frequency translation (path minus detrended trend)."""
    if len(poses) < 4:
        return 0.0
    t = np.array([pose_params(p)[:2] for p in poses])
    res = np.stack([detrend(t[:, i])[1] for i in range(2)], axis=1)
    return float(np.sqrt(np.mean((res ** 2).sum(1))))


def path_translation(poses: list[np.ndarray]) -> np.ndarray:
    return np.array([pose_params(p)[:2] for p in poses])


def compute_metrics(track_result, raw_poses, smooth_poses, smooth_info,
                    rs_info, fit_residuals, rejected_ids, pair_failures,
                    n_frames, min_points_required):
    """Assemble the metrics/diagnostics dict reported to the user."""
    counts = [track_result.active_count(k) for k in range(track_result.n_frames)]
    tracks = track_result.tracks
    lifetimes = [v["last"] - v["born"] + 1 for v in tracks.values()]
    jitter_before = jitter_px(raw_poses)
    jitter_after = jitter_px(smooth_poses)
    diagnostics = {
        "active_points_min": int(min(counts)) if counts else 0,
        "active_points_max": int(max(counts)) if counts else 0,
        "tracks_total": len(tracks),
        "track_lifetime_mean": float(np.mean(lifetimes)) if lifetimes else 0.0,
        "pair_failures": pair_failures,
        "rs_estimate": rs_info,
        "min_points_required": min_points_required,
    }
    failure_reasons = []
    if counts and min(counts) < min_points_required:
        failure_reasons.append(
            f"active points dropped to {min(counts)} (< {min_points_required})")
    failure_reasons.extend(
        f"pair {f['pair']}: {f['reason']}" for f in pair_failures)
    return {
        "frames": n_frames,
        "tracked_points": int(round(float(np.mean(counts)))) if counts else 0,
        "track_residual_px": float(np.median(fit_residuals))
        if len(fit_residuals) else None,
        "jitter_before_px": jitter_before,
        "jitter_after_px": jitter_after,
        "jitter_reduction": float(jitter_before / jitter_after)
        if jitter_after > 1e-9 else None,
        "rejected_tracks": len(rejected_ids),
        "crop_ratio": smooth_info["crop_ratio"],
        "max_correction_px": smooth_info["max_correction_px"],
        "rolling_shutter_corrected": bool(rs_info.get("applied", False)),
        "failure_reasons": failure_reasons,
        "diagnostics": diagnostics,
    }


def outlier_tracks(track_result, inlier_masks, min_outlier_ratio=0.5):
    """Track ids that are motion outliers in most pairs they join.

    `inlier_masks[k]` is the boolean inlier mask of pair k aligned with
    `track_result.matches(k)` ids.
    """
    from collections import Counter
    total = Counter()
    outliers = Counter()
    for k, mask in inlier_masks.items():
        _, _, ids = track_result.matches(k)
        for tid, inl in zip(ids, mask):
            total[tid] += 1
            if not inl:
                outliers[tid] += 1
    rejected = set()
    for tid, n in total.items():
        if n >= 3 and outliers[tid] / n >= min_outlier_ratio:
            rejected.add(tid)
    return rejected
