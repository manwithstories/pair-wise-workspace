"""NumPy-only noise-floor estimation with block quantiles and jump segmentation."""

from __future__ import annotations

import numpy as np

_FINE_BLOCK_MS = 500.0


def _sliding_median(values: np.ndarray, window: int) -> np.ndarray:
    radius = max(0, (window - 1) // 2)
    out = np.empty_like(values)
    for idx in range(values.size):
        lo = max(0, idx - radius)
        hi = min(values.size, idx + radius + 1)
        out[idx] = np.median(values[lo:hi])
    return out


def _block_quantiles(frame_db: np.ndarray, block_size: int, quantile: float,
                     cap: float | None = None) -> np.ndarray:
    nb = int(np.ceil(frame_db.size / block_size))
    floors = np.empty(nb, dtype=np.float64)
    for idx in range(nb):
        chunk = frame_db[idx * block_size:(idx + 1) * block_size]
        value = float(np.quantile(chunk, quantile))
        floors[idx] = min(value, cap) if cap is not None else value
    return floors


def estimate_noise_db(frame_db: np.ndarray, hop_ms: float, params) -> np.ndarray:
    """Per-frame noise floor in dBFS.

    Coarse ``block_ms`` (default 2 s) blocks contribute the spec-mandated 10%
    quantile room tone, median-smoothed across neighbouring blocks. A global
    low quantile anchors the steady-state level so speech-heavy blocks cannot
    inflate the floor. Fine 500 ms sub-blocks then localise a noise jump that
    falls inside a coarse block: a sub-block level is trusted as a new floor
    only when the same level persists across neighbours, otherwise it inherits
    the coarse estimate. Jumps of ``noise_jump_db`` split the timeline into
    stationary regions, each reduced to its median level.
    """
    frame_db = np.asarray(frame_db, dtype=np.float64)
    if frame_db.size == 0:
        return np.empty(0, dtype=np.float64)
    coarse_size = max(1, round(params.block_ms / hop_ms))
    fine_size = max(1, round(_FINE_BLOCK_MS / hop_ms))
    anchor = float(np.quantile(frame_db, params.noise_quantile))
    coarse = _block_quantiles(frame_db, coarse_size, params.noise_quantile,
                              cap=anchor + 2.0)
    coarse_smoothed = _sliding_median(coarse, params.noise_smooth_window)
    coarse_index = np.minimum(np.arange(frame_db.size) // coarse_size,
                              coarse.size - 1)
    coarse_per_frame = coarse_smoothed[coarse_index]

    fine = _block_quantiles(frame_db, fine_size, params.noise_quantile)
    persistent = _sliding_median(fine, params.noise_smooth_window + 2)
    trusted = np.abs(fine - persistent) <= 2.0
    fine_corrected = fine.copy()
    current = anchor
    for idx in range(fine.size):
        if trusted[idx]:
            current = fine[idx]
        fine_corrected[idx] = current
    fine_index = np.minimum(np.arange(frame_db.size) // fine_size, fine.size - 1)
    fine_per_frame = fine_corrected[fine_index]

    # The fine timeline can follow a genuine upward noise jump that the coarse
    # blocks straddle; adopt it only where it persistently exceeds the coarse
    # floor by a full jump, otherwise keep the speech-immune coarse estimate.
    jump = fine_per_frame - coarse_per_frame >= params.noise_jump_db
    estimate = np.where(jump, fine_per_frame, coarse_per_frame)
    return estimate
