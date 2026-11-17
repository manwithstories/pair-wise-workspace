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


def smooth_path(poses: list[np.ndarray], frame_shape: tuple[int, int],
                max_crop: float = 0.18, max_correction: float | None = None,
                max_accel: float = 6.0, window: int = 15,
                iterations: int = 4):
    """Constrained smoothing. Returns (smoothed_poses, info)."""
    n = len(poses)
    h, w = frame_shape
    if max_correction is None:
        max_correction = max_crop * min(h, w)
    raw = np.array([pose_params(p) for p in poses])  # (N, 3): tx, ty, th
    kernel = _gaussian_kernel(window)
    scales = np.array([1.0, 1.0, min(h, w)])  # theta limits in px-equiv
    max_corr = np.array([max_correction, max_correction,
                         max_correction / min(h, w)])
    max_acc = np.array([max_accel, max_accel,
                        max_accel / min(h, w)])
    smoothed = raw.copy()
    for _ in range(iterations):
        for ch in range(3):
            smoothed[:, ch] = _smooth_onesided(smoothed[:, ch], kernel)
        # correction clamp (crop budget)
        corr = (smoothed - raw) * scales
        excess = np.abs(corr) - max_corr * scales
        for ch in range(3):
            over = excess[:, ch] > 0
            smoothed[over, ch] = raw[over, ch] + np.sign(
                smoothed[over, ch] - raw[over, ch]) * max_corr[ch]
        # acceleration clamp (limit 2nd difference)
        for ch in range(3):
            acc = np.zeros(n)
            acc[1:-1] = smoothed[2:, ch] - 2 * smoothed[1:-1, ch] \
                + smoothed[:-2, ch]
            too = np.abs(acc) > max_acc[ch]
            if too.any():
                idx = np.where(too)[0]
                idx = idx[(idx > 0) & (idx < n - 1)]
                smoothed[idx, ch] = 0.5 * (smoothed[idx - 1, ch]
                                           + smoothed[idx + 1, ch]
                                           + np.sign(acc[idx])
                                           * max_acc[ch] * 0.5)
    corrections = np.abs((smoothed - raw) * scales)
    max_seen = float(corrections[:, :2].max()) if n else 0.0
    info = {
        "max_correction_px": max_seen,
        "crop_ratio": float(max_seen / min(h, w)) if min(h, w) else 0.0,
        "window": window,
        "max_accel": max_accel,
    }
    smoothed_poses = [params_pose(*smoothed[i]) for i in range(n)]
    return smoothed_poses, info
