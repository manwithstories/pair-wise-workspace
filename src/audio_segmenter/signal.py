"""Framing, channel merging, RMS/peak measurement and energy smoothing.

The streaming accumulator here is the memory contract of the toolkit: audio
blocks are consumed as they arrive and only one scalar per analysis frame is
retained.  Peak/RMS for whole regions are then reconstructed from those
per-frame aggregates, so memory grows with the frame grid (roughly
``samples / hop``), never with the length of the file.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Iterable, Optional

import numpy as np

EPSILON = 1e-10
DB_FLOOR = -120.0


def to_db(amplitude: float) -> float:
    """Amplitude (linear, 0..1) to dBFS with a finite floor."""

    if amplitude <= EPSILON:
        return DB_FLOOR
    return max(DB_FLOOR, 20.0 * math.log10(amplitude))


def merge_channels(samples: np.ndarray) -> np.ndarray:
    """Downmix (nframes, channels) to mono by arithmetic mean."""

    if samples.ndim == 1:
        return samples.astype(np.float32, copy=False)
    if samples.shape[1] == 1:
        return samples[:, 0].astype(np.float32, copy=False)
    return samples.mean(axis=1, dtype=np.float32)


def peak_amplitude(block: np.ndarray) -> float:
    if block.size == 0:
        return 0.0
    return float(np.max(np.abs(block)))


def rms_amplitude(block: np.ndarray) -> float:
    if block.size == 0:
        return 0.0
    return float(np.sqrt(np.mean(np.square(block, dtype=np.float64))))


@dataclass
class FrameGrid:
    """Frame geometry derived from a sample rate and millisecond settings."""

    sample_rate: int
    frame_ms: float
    hop_ms: float

    @property
    def frame_samples(self) -> int:
        return max(1, int(round(self.sample_rate * self.frame_ms / 1000.0)))

    @property
    def hop_samples(self) -> int:
        return max(1, int(round(self.sample_rate * self.hop_ms / 1000.0)))

    def frame_count(self, total_samples: int) -> int:
        if total_samples <= 0:
            return 0
        return int(math.ceil(total_samples / self.hop_samples))

    def frame_start(self, index: int) -> int:
        return index * self.hop_samples

    def frame_end(self, index: int, total_samples: int) -> int:
        return min(total_samples, self.frame_start(index) + self.frame_samples)


@dataclass
class FrameFeatures:
    """Per-frame aggregates plus the grid that produced them."""

    grid: FrameGrid
    total_samples: int
    sum_square: np.ndarray
    peak: np.ndarray
    counts: np.ndarray
    starts: np.ndarray

    @property
    def frame_count(self) -> int:
        return int(self.starts.size)

    def rms(self) -> np.ndarray:
        safe = np.maximum(self.counts, 1).astype(np.float64)
        return np.sqrt(self.sum_square / safe)

    def rms_db(self) -> np.ndarray:
        return np.array([to_db(v) for v in self.rms()], dtype=np.float64)

    def peak_db(self) -> np.ndarray:
        return np.array([to_db(v) for v in self.peak], dtype=np.float64)

    def ends(self) -> np.ndarray:
        return np.minimum(
            self.starts + self.grid.frame_samples, self.total_samples
        )


class FeatureAccumulator:
    """Consume mono sample blocks incrementally and emit FrameFeatures.

    Blocks must be 1-D mono float32 arrays.  A short buffer carries a
    partial window across block boundaries so results are identical to
    processing the whole file at once.
    """

    def __init__(self, grid: FrameGrid) -> None:
        self.grid = grid
        self._buffer = np.empty(0, dtype=np.float32)
        self._buffer_start = 0  # absolute sample index of _buffer[0]
        self._total = 0
        self._next_frame = 0
        self._sums: list[float] = []
        self._peaks: list[float] = []
        self._counts: list[int] = []
        self._starts: list[int] = []

    @property
    def total_samples(self) -> int:
        return self._total

    def _emit_ready(self, final: bool) -> None:
        frame_len = self.grid.frame_samples
        hop = self.grid.hop_samples
        while True:
            start = self._next_frame * hop
            if start >= self._total:
                return
            end = start + frame_len
            available_end = self._buffer_start + self._buffer.size
            if end > available_end and not final:
                return
            lo = start - self._buffer_start
            hi = min(end, self._total) - self._buffer_start
            if lo < 0:
                # Should not happen: data is never discarded early.
                return
            window = self._buffer[lo:hi]
            count = int(window.size)
            if count <= 0:
                return
            squares = np.square(window, dtype=np.float64)
            self._sums.append(float(np.sum(squares)))
            self._peaks.append(peak_amplitude(window))
            self._counts.append(count)
            self._starts.append(start)
            self._next_frame += 1
            self._trim(start + hop)

    def _trim(self, keep_from: int) -> None:
        drop = keep_from - self._buffer_start
        if drop > 0:
            drop = min(drop, self._buffer.size)
            self._buffer = self._buffer[drop:]
            self._buffer_start += drop

    def push(self, block: np.ndarray) -> None:
        if block.ndim != 1:
            block = block.reshape(-1)
        self._buffer = np.concatenate(
            [self._buffer, block.astype(np.float32, copy=False)]
        )
        self._total += int(block.size)
        self._emit_ready(final=False)

    def finish(self) -> FrameFeatures:
        self._emit_ready(final=True)
        grid = self.grid
        count = min(len(self._starts), grid.frame_count(self._total))
        starts = np.asarray(self._starts[:count], dtype=np.int64)
        sums = np.asarray(self._sums[:count], dtype=np.float64)
        peaks = np.asarray(self._peaks[:count], dtype=np.float64)
        counts = np.asarray(self._counts[:count], dtype=np.int64)
        return FrameFeatures(
            grid=grid,
            total_samples=self._total,
            sum_square=sums,
            peak=peaks,
            counts=counts,
            starts=starts,
        )


def extract_features(
    samples: np.ndarray, grid: FrameGrid
) -> FrameFeatures:
    """Array-oriented convenience wrapper around the accumulator."""

    mono = merge_channels(samples)
    acc = FeatureAccumulator(grid)
    block = 1 << 16
    for offset in range(0, mono.size, block):
        acc.push(mono[offset : offset + block])
    return acc.finish()


def smooth_db(values: np.ndarray, window_frames: int) -> np.ndarray:
    """Moving average with edge replication; never changes array length."""

    if values.size == 0:
        return values
    window = max(1, int(window_frames))
    if window == 1:
        return values.astype(np.float64, copy=True)
    if window > values.size:
        window = values.size
    pad_left = (window - 1) // 2
    pad_right = window - 1 - pad_left
    padded = np.pad(values, (pad_left, pad_right), mode="edge")
    kernel = np.ones(window, dtype=np.float64) / window
    return np.convolve(padded, kernel, mode="valid")


def smoothing_frames(config_smooth_ms: float, hop_ms: float) -> int:
    if hop_ms <= 0:
        return 1
    return max(1, int(round(config_smooth_ms / hop_ms)))


@dataclass
class RegionStats:
    """Peak/RMS reconstructed from per-frame aggregates."""

    start_sample: int
    end_sample: int
    peak: float
    rms: float

    @property
    def sample_count(self) -> int:
        return self.end_sample - self.start_sample


def stats_for_region(
    features: FrameFeatures, start_sample: int, end_sample: int
) -> RegionStats:
    """Merge frame aggregates overlapping [start_sample, end_sample)."""

    if end_sample <= start_sample:
        return RegionStats(start_sample, end_sample, 0.0, 0.0)
    ends = features.ends()
    mask = (ends > start_sample) & (features.starts < end_sample)
    if not np.any(mask):
        idx = int(np.argmin(np.abs(features.starts - start_sample)))
        return RegionStats(
            start_sample, end_sample, float(features.peak[idx]), to_db(0.0)
        )
    total = float(np.sum(features.sum_square[mask]))
    counts = float(np.sum(features.counts[mask]))
    peak = float(np.max(features.peak[mask]))
    rms = math.sqrt(total / counts) if counts else 0.0
    return RegionStats(start_sample, end_sample, peak, rms)


def trend_slope_db_per_frame(values: np.ndarray, window: int) -> np.ndarray:
    """Centered linear trend slope (dB per frame) over a sliding window.

    Used by ramp compensation to tell a genuine fade from a steady floor.
    """

    if values.size == 0:
        return values.astype(np.float64, copy=True)
    window = max(3, int(window))
    if window > values.size:
        window = values.size if values.size % 2 == 1 else values.size - 1
    if window < 3:
        return np.zeros_like(values, dtype=np.float64)
    half = window // 2
    padded = np.pad(values, (half, half), mode="edge")
    x = np.arange(window, dtype=np.float64)
    x = x - x.mean()
    denom = float(np.sum(x * x))
    slopes = np.empty(values.size, dtype=np.float64)
    for i in range(values.size):
        seg = padded[i : i + window]
        slopes[i] = float(np.dot(x, seg - seg.mean()) / denom)
    return slopes
