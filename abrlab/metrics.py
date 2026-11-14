"""Deterministic full-reference quality metrics."""

from __future__ import annotations

import numpy as np


class MetricsError(ValueError):
    """Raised when metric inputs are incompatible."""


def _luma(frames: np.ndarray) -> np.ndarray:
    values = frames.astype(np.float32)
    return 0.299 * values[..., 0] + 0.587 * values[..., 1] + 0.114 * values[..., 2]


def psnr(reference: np.ndarray, reconstructed: np.ndarray) -> float:
    if reference.shape != reconstructed.shape:
        raise MetricsError("PSNR inputs must have identical shapes")
    if reference.size == 0:
        raise MetricsError("PSNR inputs must not be empty")
    difference = reference.astype(np.float32) - reconstructed.astype(np.float32)
    mse = float(np.mean(difference * difference, dtype=np.float64))
    if mse <= 1e-12:
        return 99.0
    return float(min(99.0, 10.0 * np.log10((255.0 * 255.0) / mse)))


def _window_means(values: np.ndarray, window: int) -> np.ndarray:
    height, width = values.shape
    integral = np.zeros((height + 1, width + 1), dtype=np.float64)
    integral[1:, 1:] = values.astype(np.float64).cumsum(axis=0).cumsum(axis=1)
    sums = (
        integral[window:, window:]
        - integral[:-window, window:]
        - integral[window:, :-window]
        + integral[:-window, :-window]
    )
    return sums / float(window * window)


def _frame_ssim(reference: np.ndarray, reconstructed: np.ndarray) -> float:
    height, width = reference.shape
    window = min(7, height, width)
    if window < 2:
        x_mean = float(np.mean(reference))
        y_mean = float(np.mean(reconstructed))
        x_var = float(np.var(reference))
        y_var = float(np.var(reconstructed))
        covariance = float(np.mean((reference - x_mean) * (reconstructed - y_mean)))
    else:
        x_mean = _window_means(reference, window)
        y_mean = _window_means(reconstructed, window)
        x_var = _window_means(reference * reference, window) - x_mean * x_mean
        y_var = _window_means(reconstructed * reconstructed, window) - y_mean * y_mean
        covariance = _window_means(reference * reconstructed, window) - x_mean * y_mean
        x_mean = x_mean.ravel()
        y_mean = y_mean.ravel()
        x_var = x_var.ravel()
        y_var = y_var.ravel()
        covariance = covariance.ravel()
    c1 = (0.01 * 255.0) ** 2
    c2 = (0.03 * 255.0) ** 2
    numerator = (2.0 * x_mean * y_mean + c1) * (2.0 * covariance + c2)
    denominator = (x_mean * x_mean + y_mean * y_mean + c1) * (x_var + y_var + c2)
    if np.any(denominator <= 0.0):
        return 1.0 if np.array_equal(reference, reconstructed) else 0.0
    result = float(np.mean(numerator / denominator))
    return float(np.clip(result, 0.0, 1.0))


def ssim(reference: np.ndarray, reconstructed: np.ndarray) -> float:
    if reference.shape != reconstructed.shape:
        raise MetricsError("SSIM inputs must have identical shapes")
    if reference.ndim != 4 or reference.shape[-1] != 3:
        raise MetricsError("SSIM inputs must have shape (frames, height, width, 3)")
    reference_luma = _luma(reference)
    reconstructed_luma = _luma(reconstructed)
    scores = [
        _frame_ssim(reference_luma[index], reconstructed_luma[index])
        for index in range(reference.shape[0])
    ]
    if not scores:
        raise MetricsError("SSIM inputs must contain at least one frame")
    return float(np.mean(scores))


def evaluate(reference: np.ndarray, reconstructed: np.ndarray) -> dict[str, float]:
    return {
        "psnr_db": round(psnr(reference, reconstructed), 6),
        "ssim": round(ssim(reference, reconstructed), 6),
    }
