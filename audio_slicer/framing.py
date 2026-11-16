"""Frame-level energy: fixed-size frames, RMS converted to dBFS."""
from __future__ import annotations

import numpy as np

DB_FLOOR = -120.0
_EPS = 1e-6


def frame_dbfs(samples, sample_rate, frame_ms=10.0):
    """Return (dBFS per frame, frame length in samples).

    Processes the signal in blocks so peak memory stays bounded by the
    signal itself plus a small working buffer.
    """
    if frame_ms <= 0:
        raise ValueError("frame_ms must be positive")
    frame_len = max(1, int(round(sample_rate * frame_ms / 1000.0)))
    total = samples.shape[0]
    n_frames = (total + frame_len - 1) // frame_len
    rms = np.empty(n_frames, dtype=np.float64)
    block = frame_len * 256
    idx = 0
    for off in range(0, total, block):
        chunk = samples[off:off + block]
        n_full = chunk.shape[0] // frame_len
        if n_full:
            grid = chunk[:n_full * frame_len].reshape(n_full, frame_len)
            rms[idx:idx + n_full] = np.sqrt(np.mean(grid * grid, axis=1))
            idx += n_full
        tail = chunk[n_full * frame_len:]
        if tail.shape[0]:
            rms[idx] = np.sqrt(np.mean(tail * tail))
            idx += 1
    db = 20.0 * np.log10(np.maximum(rms, _EPS))
    return np.maximum(db, DB_FLOOR), frame_len
