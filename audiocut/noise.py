"""Noise-floor estimation: chunked 10th percentile + median smoothing."""

from __future__ import annotations

import numpy as np

from .framing import FLOOR_DB


def _median_smooth(values: np.ndarray, kernel: int) -> np.ndarray:
    """Median filter with shrinking windows at the edges (no padding)."""
    if kernel <= 1 or values.size < 3:
        return values
    half = kernel // 2
    out = np.empty_like(values)
    for i in range(values.size):
        out[i] = np.median(values[max(0, i - half): i + half + 1])
    return out


def estimate_noise_floor(env_db: np.ndarray, chunk_frames: int = 50,
                         kernel: int = 5) -> np.ndarray:
    """Per-frame noise floor in dBFS.

    Splits the envelope into chunks, takes the 10th percentile per chunk,
    median-smooths across chunks, then interpolates between chunk centers.
    Degenerate inputs (all silence / all speech) fall back to the global
    10th percentile so no fixed absolute threshold is ever used.
    """
    n = env_db.size
    if n == 0:
        return np.empty(0)
    if np.all(env_db <= FLOOR_DB + 1.0):  # all-silence degenerate branch
        return np.full(n, FLOOR_DB)
    if float(env_db.max() - env_db.min()) < 3.0:
        # Constant-signal degenerate branch: a steady low level is a silent
        # (noise-only) file; a steady high level is continuous speech.
        if float(env_db.mean()) < -50.0:
            return np.array(env_db, copy=True)
        return np.full(n, float(env_db.min()) - 12.0)
    # Gate out frames clearly above the noise band so speech cannot
    # contaminate the per-chunk percentiles.
    anchor = float(np.percentile(env_db, 10))
    gated = np.where(env_db <= anchor + 15.0, env_db, np.nan)
    n_chunks = max(1, (n + chunk_frames - 1) // chunk_frames)
    levels = np.empty(n_chunks)
    for i in range(n_chunks):
        vals = gated[i * chunk_frames:(i + 1) * chunk_frames]
        vals = vals[~np.isnan(vals)]
        levels[i] = np.percentile(vals, 10) if vals.size >= 3 else np.nan
    valid = ~np.isnan(levels)
    if not valid.any():
        return np.full(n, anchor)
    if not valid.all():
        levels = np.interp(np.arange(n_chunks), np.flatnonzero(valid),
                           levels[valid])
    levels = _median_smooth(levels, kernel)
    centers = np.minimum(np.arange(n_chunks) * chunk_frames + chunk_frames / 2,
                         n - 1)
    return np.interp(np.arange(n), centers, levels)
