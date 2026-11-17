"""Rolling-shutter row-timing model.

Each row r of a frame is exposed at t + rs * direction * frac(r), where
frac(r) = r/(H-1) - 0.5. A global per-frame motion fit explains the
center-time motion; the per-band residual translation is, to first
order, linear in frac(r) with slope = content_velocity * rs * direction.

Per-frame slopes are noisy, so the readout parameters are estimated
globally (robust median of slope/velocity ratios over all frames), and
the correction applied per frame is model-based:
offset_k(r) = v_k * rs * direction * frac(r).
"""

from __future__ import annotations

import numpy as np


def row_frac(n_rows: int) -> np.ndarray:
    return np.arange(n_rows, dtype=np.float64) / max(n_rows - 1, 1) - 0.5


def _robust_slope(frac: np.ndarray, values: np.ndarray,
                  iters: int = 3) -> float:
    """IRLS fit of values ~ intercept + slope*frac (intercept centered out,
    so asymmetric/missing bands cannot leak the mean into the slope)."""
    if len(frac) < 2 or np.allclose(frac, 0):
        return 0.0
    w = np.ones(len(frac))
    slope = 0.0
    for _ in range(iters):
        wsum = w.sum()
        if wsum < 1e-12:
            return 0.0
        fm = float((w * frac).sum() / wsum)
        vm = float((w * values).sum() / wsum)
        fc = frac - fm
        denom = float((w * fc * fc).sum())
        if denom < 1e-12:
            return 0.0
        slope = float((w * fc * (values - vm)).sum() / denom)
        r = values - (vm + slope * fc)
        s = max(1.4826 * np.median(np.abs(r)), 0.5)
        w = np.minimum(1.0, 2.0 * s / np.maximum(np.abs(r), 1e-9))
    return slope


def band_slopes(band_centers: np.ndarray, band_offsets: np.ndarray,
                band_counts: np.ndarray, n_rows: int,
                min_band_points: int = 8) -> np.ndarray:
    """Robust per-frame slope of band offsets vs row frac, per axis."""
    ok = band_counts >= min_band_points
    if ok.sum() < 3:
        return np.zeros(2)
    band_frac = band_centers[ok] / max(n_rows - 1, 1) - 0.5
    return np.array([_robust_slope(band_frac, band_offsets[ok, 0]),
                     _robust_slope(band_frac, band_offsets[ok, 1])])


def estimate_parameters(slopes: list[np.ndarray],
                        velocities: list[np.ndarray],
                        n_rows: int, assumed_direction: int = 1,
                        min_accel: float = 2.0) -> dict:
    """Estimate row delay (frame periods) and exposure direction.

    Both frames of a pair share the same readout, so the row-dependent
    part of a measured displacement field is the *difference* of the two
    frames' jello shifts. Frame j's jello is driven by the camera
    velocity between j and j+1, hence pair k's displacement slope is
    slope_k = rs * (u_{k+1} - u_k) * direction, with u the content
    velocity from the global fits (velocities[i] = pair i -> i+1).
    Each pair/axis with enough acceleration votes; the median rules.
    """
    votes = []
    for k in range(len(slopes) - 1):
        du = np.asarray(velocities[k + 1]) - np.asarray(velocities[k])
        for ax in range(2):
            if abs(du[ax]) < min_accel:
                continue
            rs = slopes[k][ax] / (du[ax] * assumed_direction)
            if -1.5 < rs < 1.5:
                votes.append(rs)
    if not votes:
        return {"row_delay": 0.0, "direction": int(assumed_direction),
                "significant": False, "samples": 0}
    rs = float(np.median(votes))
    direction = int(assumed_direction)
    if rs < 0:
        direction = -int(assumed_direction)
        rs = -rs
    return {"row_delay": float(rs), "direction": direction,
            "significant": bool(rs > 0.02), "samples": len(votes)}


def model_row_offsets(velocity: np.ndarray, row_delay: float,
                      direction: int, n_rows: int) -> np.ndarray:
    """Model-based per-row offset: v * rs * direction * frac(r)."""
    frac = row_frac(n_rows)
    return np.asarray(velocity, dtype=np.float64)[None, :] \
        * (row_delay * direction) * frac[:, None]


def correct_points(xy: np.ndarray, row_offsets: np.ndarray) -> np.ndarray:
    """Shift measured points to center-time coordinates."""
    if len(xy) == 0:
        return xy
    rows = np.clip(np.round(xy[:, 1]).astype(int), 0, len(row_offsets) - 1)
    return xy - row_offsets[rows]


def correct_frame(img: np.ndarray, row_offsets: np.ndarray,
                  fill: float = 0.0) -> np.ndarray:
    """Undo per-row displacement: out(r, c) = img(r + dy(r), c + dx(r))."""
    from .frameio import bilinear_sample
    h, w = img.shape
    yy, xx = np.mgrid[0:h, 0:w].astype(np.float64)
    yy += row_offsets[:, 1:2]
    xx += row_offsets[:, 0:1]
    return bilinear_sample(img, yy, xx, fill=fill)
