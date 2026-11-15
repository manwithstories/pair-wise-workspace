"""Robust noise-floor estimation and adaptive hysteresis thresholds."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np


@dataclass(frozen=True)
class Thresholds:
    noise_floor_dbfs: float
    enter_dbfs: float
    exit_dbfs: float


def _mode(values: np.ndarray, bin_width: float = 0.5) -> float:
    if values.size == 0:
        return -120.0
    low, high = float(values.min()), float(values.max())
    if high - low < bin_width:
        return float(values.mean())
    bins = max(4, min(256, int(np.ceil((high - low) / bin_width)) + 1))
    counts, edges = np.histogram(values, bins=bins)
    index = int(np.argmax(counts))
    return float((edges[index] + edges[index + 1]) * 0.5)


def estimate_noise_floor(
    rms_dbfs: np.ndarray,
    zcr: np.ndarray | None = None,
    enter_db: float = 6.0,
) -> float:
    """Estimate the floor from the low quantile and its histogram mode."""

    values = np.asarray(rms_dbfs, dtype=np.float64)
    values = values[np.isfinite(values)]
    if values.size == 0:
        return -120.0
    p5, p10, p50, p95 = np.percentile(values, [5, 10, 50, 95])
    low = values[values <= p5]
    floor = np.clip(_mode(low), -120.0, 0.0)

    # A stationary, low-frequency signal has no noise-only frames to learn
    # from. Keep it active while still treating stationary broadband hiss as
    # silence. Digital silence remains below any non-negative threshold.
    if p95 - p10 < 3.0:
        median_zcr = float(np.median(zcr)) if zcr is not None and zcr.size else 1.0
        if p50 <= -75.0 or median_zcr >= 0.15 or median_zcr < 0.001:
            floor = float(p50)
        else:
            floor = float(p10 - max(12.0, enter_db + 6.0))
    return float(np.clip(floor, -120.0, 0.0))


def build_thresholds(
    noise_floor_dbfs: float, enter_db: float, exit_db: float
) -> Thresholds:
    return Thresholds(
        noise_floor_dbfs=float(noise_floor_dbfs),
        enter_dbfs=float(noise_floor_dbfs + enter_db),
        exit_dbfs=float(noise_floor_dbfs + exit_db),
    )

