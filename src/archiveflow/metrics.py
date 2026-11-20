"""Quality, bitrate and alignment statistics (NumPy)."""

from __future__ import annotations

import math

import numpy as np


def psnr_db(reference: np.ndarray, test: np.ndarray) -> float:
    """Peak signal-to-noise ratio between two equally shaped uint8 frames."""
    ref = reference.astype(np.float64)
    tst = test.astype(np.float64)
    mse = float(np.mean((ref - tst) ** 2))
    if mse <= 1e-12:
        return 99.0  # lossless for practical purposes
    return 10.0 * math.log10(255.0 * 255.0 / mse)


def ssim(reference: np.ndarray, test: np.ndarray) -> float:
    """Simplified global SSIM on luma (8-bit frames, any channel count)."""
    ref = _luma(reference)
    tst = _luma(test)
    c1 = (0.01 * 255.0) ** 2
    c2 = (0.03 * 255.0) ** 2
    mu_r, mu_t = float(ref.mean()), float(tst.mean())
    var_r, var_t = float(ref.var()), float(tst.var())
    cov = float(((ref - mu_r) * (tst - mu_t)).mean())
    denom = (mu_r**2 + mu_t**2 + c1) * (var_r + var_t + c2)
    if denom <= 0:
        return 1.0
    return ((2 * mu_r * mu_t + c1) * (2 * cov + c2)) / denom


def _luma(frame: np.ndarray) -> np.ndarray:
    f = frame.astype(np.float64)
    if f.ndim == 3 and f.shape[2] >= 3:
        return 0.299 * f[..., 0] + 0.587 * f[..., 1] + 0.114 * f[..., 2]
    return f


def mean(values: list[float]) -> float:
    return float(np.mean(values)) if values else 0.0


def bitrate_error_pct(actual_bps: float, target_bps: int) -> float:
    if target_bps <= 0:
        return 0.0
    return (actual_bps - target_bps) / target_bps * 100.0


def summarize_quality(per_frame_psnr: list[float],
                      per_frame_ssim: list[float]) -> dict:
    return {
        "psnr_db": round(mean(per_frame_psnr), 3),
        "ssim": round(mean(per_frame_ssim), 4),
    }
