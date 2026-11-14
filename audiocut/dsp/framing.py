"""Frame blocking and per-frame energy statistics.

Memory stays bounded: the sliding window is a strided view and RMS /
peak are reduced in blocks, so a 40 min 48 kHz mono take (~460 MB as
float32) never materialises a frames x window matrix.
"""
from __future__ import annotations

from dataclasses import dataclass

import numpy as np
from numpy.lib.stride_tricks import sliding_window_view

_BLOCK = 4096
_DB_FLOOR = 1e-7  # -140 dBFS


@dataclass
class FrameStats:
    rms_db: np.ndarray  # per-frame RMS in dBFS, float32
    peak: np.ndarray    # per-frame peak amplitude, float32
    frame_len: int      # samples per frame
    hop: int            # samples between frame starts
    sample_rate: int

    @property
    def n_frames(self) -> int:
        return int(self.rms_db.shape[0])


def _to_db(values: np.ndarray) -> np.ndarray:
    return 20.0 * np.log10(np.maximum(values, _DB_FLOOR))


def frame_stats(samples: np.ndarray, sample_rate: int,
                frame_ms: float = 25.0, hop_ms: float = 10.0) -> FrameStats:
    frame_len = max(1, int(round(sample_rate * frame_ms / 1000.0)))
    hop = max(1, int(round(sample_rate * hop_ms / 1000.0)))
    n = int(samples.shape[0])
    if n < frame_len:
        rms = float(np.sqrt(np.mean(samples.astype(np.float64) ** 2))) if n else 0.0
        peak = float(np.max(np.abs(samples))) if n else 0.0
        return FrameStats(_to_db(np.array([rms], dtype=np.float32)),
                          np.array([peak], dtype=np.float32),
                          frame_len, hop, sample_rate)
    n_frames = 1 + (n - frame_len) // hop
    windows = sliding_window_view(samples, frame_len)[::hop][:n_frames]
    rms = np.empty(n_frames, dtype=np.float32)
    peak = np.empty(n_frames, dtype=np.float32)
    for lo in range(0, n_frames, _BLOCK):
        blk = windows[lo:lo + _BLOCK]
        sq = np.einsum("ij,ij->i", blk, blk)
        rms[lo:lo + _BLOCK] = np.sqrt(sq / frame_len)
        peak[lo:lo + _BLOCK] = np.abs(blk).max(axis=1)
    return FrameStats(_to_db(rms).astype(np.float32), peak,
                      frame_len, hop, sample_rate)
