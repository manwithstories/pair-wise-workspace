"""Camera-path smoothing with crop and acceleration constraints.

The raw path is decomposed into (tx, ty, theta); each channel is
Gaussian-smoothed with one-sided (renormalized, truncated) windows at
the sequence ends, then clamped so the correction stays inside the
crop budget and the smoothed path's acceleration stays bounded.
"""

from __future__ import annotations

import numpy as np

from .motion import pose_params, params_pose


def _gaussian_kernel(window: int) -> np.ndarray:
    sigma = max(window / 3.0, 1.0)
    x = np.arange(window) - window // 2
    k = np.exp(-0.5 * (x / sigma) ** 2)
    return k / k.sum()


def _smooth_onesided(signal: np.ndarray, kernel: np.ndarray) -> np.ndarray:
    """Gaussian smooth; at the boundaries the kernel is truncated to the
    available (one-sided) samples and renormalized."""
    n = len(signal)
    half = len(kernel) // 2
    out = np.empty(n)
    for i in range(n):
        lo = max(0, i - half)
        hi = min(n, i + half + 1)
        k = kernel[lo - (i - half): hi - (i - half)]
        k = k / k.sum()
        out[i] = float((signal[lo:hi] * k).sum())
    return out


def detrend(signal: np.ndarray, poly_deg: int = 2,
            window: int = 31) -> tuple[np.ndarray, np.ndarray]:
    """Split signal into (trend, residual): low-order polynomial plus a
    heavy one-sided Gaussian on the remainder. Boundary-safe: the
    polynomial absorbs ramps, so the one-sided window never lags."""
    n = len(signal)
    t = np.arange(n, dtype=np.float64)
    coeffs = np.polyfit(t, signal, min(poly_deg, max(n - 1, 0)))
    poly = np.polyval(coeffs, t)
    rest = signal - poly
    kernel = _gaussian_kernel(min(window, max(3, n // 2 * 2 + 1)))
    trend = poly + _smooth_onesided(rest, kernel)
    return trend, signal - trend


def smooth_path(poses: list[np.ndarray], frame_shape: tuple[int, int],
                max_crop: float = 0.18, max_correction: float | None = None,
                max_accel: float = 6.0, window: int = 21,
                iterations: int = 4):
    """Constrained smoothing. Returns (smoothed_poses, info)."""
    n = len(poses)
    h, w = frame_shape
    if max_correction is None:
        max_correction = max_crop * min(h, w)
    raw = np.array([pose_params(p) for p in poses])  # (N, 3): tx, ty, th
    kernel = _gaussian_kernel(window)
    max_corr = np.array([max_correction, max_correction,
                         max_correction / min(h, w)])
    max_acc = np.array([max_accel, max_accel,
                        max_accel / min(h, w)])
    # detrend: intentional motion (pan/drift) is preserved exactly;
    # only the residual (jitter) is smoothed, with one-sided windows
    trends = np.zeros_like(raw)
    resid = np.zeros_like(raw)
    for ch in range(3):
        trends[:, ch], resid[:, ch] = detrend(raw[:, ch])
    sm_resid = resid.copy()
    for _ in range(iterations):
        for ch in range(3):
            sm_resid[:, ch] = _smooth_onesided(sm_resid[:, ch], kernel)
        # acceleration clamp on the smoothed residual
        for ch in range(3):
            acc = np.zeros(n)
            acc[1:-1] = sm_resid[2:, ch] - 2 * sm_resid[1:-1, ch] \
                + sm_resid[:-2, ch]
            idx = np.where(np.abs(acc) > max_acc[ch])[0]
            idx = idx[(idx > 0) & (idx < n - 1)]
            if len(idx):
                sm_resid[idx, ch] = 0.5 * (
                    sm_resid[idx - 1, ch] + sm_resid[idx + 1, ch]
                    - np.sign(acc[idx]) * max_acc[ch])
        # correction clamp (crop budget) — last, so it always holds
        for ch in range(3):
            corr = sm_resid[:, ch] - resid[:, ch]
            over = np.abs(corr) > max_corr[ch]
            sm_resid[over, ch] = resid[over, ch] + np.sign(
                corr[over]) * max_corr[ch]
    smoothed = trends + sm_resid
    corrections = np.abs((smoothed - raw) * np.array([1.0, 1.0, min(h, w)]))
    max_seen = float(corrections[:, :2].max()) if n else 0.0
    info = {
        "max_correction_px": max_seen,
        "crop_ratio": float(max_seen / min(h, w)) if min(h, w) else 0.0,
        "window": window,
        "max_accel": max_accel,
    }
    smoothed_poses = [params_pose(*smoothed[i]) for i in range(n)]
    return smoothed_poses, info
