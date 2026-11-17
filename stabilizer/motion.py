"""Robust motion estimation and camera-path assembly.

Affine model via iteratively reweighted least squares (Huber), so
moving foreground, mismatches and fast-pan outliers never abort the
sequence. Piecewise (per row-band) residual translation feeds the
rolling-shutter model.
"""

from __future__ import annotations

import numpy as np

from . import InsufficientTracksError, NumericalError

MIN_INLIERS = 6


def estimate_affine(src: np.ndarray, dst: np.ndarray,
                    w: np.ndarray | None = None) -> np.ndarray:
    """Weighted least-squares 2x3 affine mapping src -> dst."""
    n = len(src)
    if n < 3:
        raise NumericalError("need >=3 points for affine fit",
                             {"points": n})
    x, y = src[:, 0], src[:, 1]
    a = np.zeros((2 * n, 6))
    a[0::2] = np.stack([x, y, np.ones(n), np.zeros(n), np.zeros(n),
                        np.zeros(n)], axis=1)
    a[1::2] = np.stack([np.zeros(n), np.zeros(n), np.zeros(n), x, y,
                        np.ones(n)], axis=1)
    b = np.empty(2 * n)
    b[0::2] = dst[:, 0]
    b[1::2] = dst[:, 1]
    if w is not None:
        sw = np.sqrt(np.repeat(w, 2))[:, None]
        a = a * sw
        b = b * sw[:, 0]
    try:
        sol, *_ = np.linalg.lstsq(a, b, rcond=None)
    except np.linalg.LinAlgError as exc:
        raise NumericalError("singular affine system",
                             {"points": n}) from exc
    if not np.isfinite(sol).all():
        raise NumericalError("non-finite affine solution", {"points": n})
    return sol.reshape(2, 3)


def robust_affine(src: np.ndarray, dst: np.ndarray, iters: int = 6,
                  huber_c: float = 1.5, min_points: int = MIN_INLIERS):
    """IRLS affine fit. Returns (M, inlier_mask, weights, rms_px)."""
    n = len(src)
    if n < min_points:
        raise InsufficientTracksError(
            "too few correspondences for motion estimation",
            {"correspondences": n, "required": min_points})
    w = np.ones(n)
    m = estimate_affine(src, dst, w)
    for _ in range(iters):
        pred = src @ m[:, :2].T + m[:, 2]
        r = np.hypot(*(dst - pred).T)
        s = max(1.4826 * np.median(r), 0.5)
        w = np.where(r <= huber_c * s, 1.0, huber_c * s / np.maximum(r, 1e-9))
        m = estimate_affine(src, dst, w)
    pred = src @ m[:, :2].T + m[:, 2]
    r = np.hypot(*(dst - pred).T)
    s = max(1.4826 * np.median(r), 0.5)
    inliers = r <= 2.5 * s
    if inliers.sum() < min_points:
        raise InsufficientTracksError(
            "too few inliers after robust fit",
            {"inliers": int(inliers.sum()), "required": min_points})
    m = estimate_affine(src[inliers], dst[inliers])
    pred = src[inliers] @ m[:, :2].T + m[:, 2]
    rms = float(np.sqrt(np.mean(((dst[inliers] - pred) ** 2).sum(1))))
    return m, inliers, w, rms


def band_translations(src: np.ndarray, dst: np.ndarray, m: np.ndarray,
                      n_bands: int = 4, frame_h: int | None = None):
    """Median residual translation per row band (piecewise motion).

    Returns (band_centers_y, offsets (n_bands, 2), counts).
    """
    pred = src @ m[:, :2].T + m[:, 2]
    resid = dst - pred
    h = frame_h if frame_h else int(src[:, 1].max()) + 1
    edges = np.linspace(0, h, n_bands + 1)
    centers = 0.5 * (edges[:-1] + edges[1:])
    offsets = np.zeros((n_bands, 2))
    counts = np.zeros(n_bands, dtype=int)
    for b in range(n_bands):
        mask = (src[:, 1] >= edges[b]) & (src[:, 1] < edges[b + 1])
        counts[b] = int(mask.sum())
        if counts[b] >= 3:
            offsets[b] = np.median(resid[mask], axis=0)
    return centers, offsets, counts


def compose_path(transforms: list[np.ndarray]) -> list[np.ndarray]:
    """Cumulative 3x3 poses; T_k maps frame k-1 coords into frame k."""
    poses = [np.eye(3)]
    for t in transforms:
        t3 = np.eye(3)
        t3[:2] = t
        poses.append(t3 @ poses[-1])
    return poses


def pose_params(pose: np.ndarray):
    """(tx, ty, theta) of a 3x3 pose (translation of the origin + rot)."""
    theta = float(np.arctan2(pose[1, 0], pose[0, 0]))
    return float(pose[0, 2]), float(pose[1, 2]), theta


def params_pose(tx: float, ty: float, theta: float) -> np.ndarray:
    c, s = np.cos(theta), np.sin(theta)
    p = np.eye(3)
    p[:2] = [[c, -s, tx], [s, c, ty]]
    return p
