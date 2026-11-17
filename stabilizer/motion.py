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


def estimate_euclidean(src: np.ndarray, dst: np.ndarray,
                       w: np.ndarray | None = None) -> np.ndarray:
    """Weighted Kabsch fit: rotation + translation as a 2x3 matrix."""
    n = len(src)
    if n < 2:
        raise NumericalError("need >=2 points for euclidean fit",
                             {"points": n})
    if w is None:
        w = np.ones(n)
    wsum = w.sum()
    if wsum <= 0:
        raise NumericalError("degenerate weights", {"points": n})
    cs = (src * w[:, None]).sum(0) / wsum
    cd = (dst * w[:, None]).sum(0) / wsum
    xs = src - cs
    xd = dst - cd
    cov = (xs * w[:, None]).T @ xd
    try:
        u, _, vt = np.linalg.svd(cov)
    except np.linalg.LinAlgError as exc:
        raise NumericalError("singular euclidean system",
                             {"points": n}) from exc
    r = u @ vt
    if np.linalg.det(r) < 0:
        u[:, -1] *= -1
        r = u @ vt
    t = cd - r @ cs
    m = np.empty((2, 3))
    m[:, :2] = r
    m[:, 2] = t
    return m


def robust_euclidean(src: np.ndarray, dst: np.ndarray, iters: int = 6,
                     huber_c: float = 1.5, min_points: int = MIN_INLIERS):
    """IRLS rotation+translation fit. Returns (M, inliers, weights, rms)."""
    n = len(src)
    if n < min_points:
        raise InsufficientTracksError(
            "too few correspondences for motion estimation",
            {"correspondences": n, "required": min_points})
    w = np.ones(n)
    m = estimate_euclidean(src, dst, w)
    for _ in range(iters):
        pred = src @ m[:, :2].T + m[:, 2]
        r = np.hypot(*(dst - pred).T)
        s = max(1.4826 * np.median(r), 0.5)
        w = np.where(r <= huber_c * s, 1.0, huber_c * s / np.maximum(r, 1e-9))
        m = estimate_euclidean(src, dst, w)
    pred = src @ m[:, :2].T + m[:, 2]
    r = np.hypot(*(dst - pred).T)
    s = max(1.4826 * np.median(r), 0.5)
    inliers = r <= 2.5 * s
    if inliers.sum() < min_points:
        raise InsufficientTracksError(
            "too few inliers after robust fit",
            {"inliers": int(inliers.sum()), "required": min_points})
    m = estimate_euclidean(src[inliers], dst[inliers])
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


def sanitize_transforms(transforms: list[np.ndarray],
                        residuals: list[float], max_rms: float = 3.0,
                        max_rot_deg: float = 2.5,
                        max_scale_dev: float = 0.05, window: int = 5):
    """Flag geometrically implausible pairs and repair them.

    A pair whose robust fit still shows high RMS, wild rotation or zoom
    is dominated by mismatches; keeping it would poison the camera path.
    Failed pairs are replaced by the component-wise median of nearby
    good pairs (sequence is never aborted). Returns (cleaned, failures).
    """
    params = []
    for m in transforms:
        theta = float(np.arctan2(m[1, 0], m[0, 0]))
        scale = float(np.sqrt(max(np.linalg.det(m[:, :2]), 1e-12)))
        params.append([m[0, 2], m[1, 2], theta, scale])
    params = np.array(params)
    failures = []
    good = np.ones(len(transforms), dtype=bool)
    for i, (p, rms) in enumerate(zip(params, residuals)):
        why = []
        if rms > max_rms:
            why.append(f"rms={rms:.2f}")
        if abs(np.rad2deg(p[2])) > max_rot_deg:
            why.append(f"rot={np.rad2deg(p[2]):.2f}deg")
        if abs(p[3] - 1.0) > max_scale_dev:
            why.append(f"scale={p[3]:.3f}")
        if why:
            good[i] = False
            failures.append({"pair": i, "reason": ";".join(why)})
    cleaned = [m.copy() for m in transforms]
    for f in failures:
        i = f["pair"]
        lo, hi = max(0, i - window), min(len(params), i + window + 1)
        ref = np.median(params[lo:hi][good[lo:hi]], axis=0) \
            if good[lo:hi].any() else np.median(params, axis=0)
        c, s = np.cos(ref[2]), np.sin(ref[2])
        cleaned[i] = np.array([[c * ref[3], -s * ref[3], ref[0]],
                               [s * ref[3], c * ref[3], ref[1]]])
    return cleaned, failures


def compose_path(transforms: list[np.ndarray]) -> list[np.ndarray]:    """Cumulative 3x3 poses; T_k maps frame k-1 coords into frame k."""
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
