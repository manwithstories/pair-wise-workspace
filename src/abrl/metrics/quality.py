"""PSNR / SSIM quality metrics with frame-level aggregation."""
from __future__ import annotations

import numpy as np

PSNR_IDENTICAL = 99.0
_SSIM_WINDOW = 8
_SSIM_C1 = (0.01 * 255) ** 2
_SSIM_C2 = (0.03 * 255) ** 2


def psnr(reference: np.ndarray, distorted: np.ndarray) -> float:
    """Luma PSNR in dB; identical frames return the 99.0 sentinel."""
    if reference.shape != distorted.shape:
        raise ValueError(f"shape mismatch {reference.shape} vs {distorted.shape}")
    diff = reference.astype(np.float64) - distorted.astype(np.float64)
    mse = float(np.mean(diff * diff))
    if mse == 0.0:
        return PSNR_IDENTICAL
    return float(10.0 * np.log10((255.0 ** 2) / mse))


def _window_stats(plane: np.ndarray):
    """Per-pixel local mean/variance/covariance over 8x8 windows with edge pad."""
    padded = np.pad(plane, _SSIM_WINDOW // 2, mode="edge").astype(np.float64)
    integral = np.pad(padded, ((1, 0), (1, 0)), mode="constant").cumsum(0).cumsum(1)
    h, w = plane.shape
    r0 = np.arange(h)[:, None]
    c0 = np.arange(w)[None, :]
    total = (
        integral[r0 + _SSIM_WINDOW, c0 + _SSIM_WINDOW]
        - integral[r0, c0 + _SSIM_WINDOW]
        - integral[r0 + _SSIM_WINDOW, c0]
        + integral[r0, c0]
    )
    return total / (_SSIM_WINDOW * _SSIM_WINDOW)


def ssim(reference: np.ndarray, distorted: np.ndarray) -> float:
    """Mean SSIM using 8x8 uniform windows with reflect/edge padding, 4 decimals."""
    if reference.shape != distorted.shape:
        raise ValueError(f"shape mismatch {reference.shape} vs {distorted.shape}")
    ref = reference.astype(np.float64)
    dst = distorted.astype(np.float64)
    mu_x = _window_stats(ref)
    mu_y = _window_stats(dst)
    var_x = _window_stats(ref * ref) - mu_x * mu_x
    var_y = _window_stats(dst * dst) - mu_y * mu_y
    cov_xy = _window_stats(ref * dst) - mu_x * mu_y
    var_x = np.maximum(var_x, 0.0)
    var_y = np.maximum(var_y, 0.0)
    numerator = (2 * mu_x * mu_y + _SSIM_C1) * (2 * cov_xy + _SSIM_C2)
    denominator = (mu_x ** 2 + mu_y ** 2 + _SSIM_C1) * (var_x + var_y + _SSIM_C2)
    return float(np.round(np.mean(numerator / denominator), 4))


def frame_psnr(reference, distorted) -> float:
    return psnr(reference.y, distorted.y)


def frame_ssim(reference, distorted) -> float:
    return ssim(reference.y, distorted.y)


def aggregate(reference_frames, distorted_frames) -> dict:
    """Per-frame PSNR/SSIM plus mean/min aggregation over a frame pair list."""
    if len(reference_frames) != len(distorted_frames):
        raise ValueError("frame count mismatch")
    per_psnr = [frame_psnr(a, b) for a, b in zip(reference_frames, distorted_frames)]
    per_ssim = [frame_ssim(a, b) for a, b in zip(reference_frames, distorted_frames)]
    real = [value for value in per_psnr if value != PSNR_IDENTICAL]
    mean_psnr = PSNR_IDENTICAL if not real else float(np.mean(real))
    return {
        "frames": len(per_psnr),
        "psnr_db": round(mean_psnr, 4),
        "psnr_min_db": round(float(min(per_psnr)), 4),
        "ssim": round(float(np.mean(per_ssim)), 4),
        "ssim_min": round(float(min(per_ssim)), 4),
        "per_frame_psnr": [round(v, 4) for v in per_psnr],
        "per_frame_ssim": [round(v, 4) for v in per_ssim],
    }
