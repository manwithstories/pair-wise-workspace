"""Small, deterministic rational resampler with cross-block history."""

from __future__ import annotations

from dataclasses import dataclass
import math
from typing import Callable

import numpy as np

from .errors import ManifestError


NativeReader = Callable[[int, int], np.ndarray]


@dataclass(frozen=True)
class ResampleRatio:
    numerator: int
    denominator: int

    def as_dict(self) -> dict[str, int]:
        return {"numerator": self.numerator, "denominator": self.denominator}

    def __str__(self) -> str:
        return f"{self.numerator}/{self.denominator}"


class RationalResampler:
    """Rational sample-rate conversion using a normalized windowed-sinc FIR.

    Output sample *n* maps to source position ``n * denominator / numerator``.
    Every block computes from absolute positions, so callers can split output
    at any boundary. A native-sample cache keeps FIR look-behind/look-ahead
    history alive across pulls without retaining the whole source.
    """

    def __init__(
        self,
        source_rate: int,
        target_rate: int,
        *,
        taps: int = 17,
        field: str = "resample",
    ) -> None:
        if not isinstance(source_rate, int) or source_rate <= 0:
            raise ManifestError("source sample rate must be a positive integer", field)
        if not isinstance(target_rate, int) or target_rate <= 0:
            raise ManifestError("target sample rate must be a positive integer", field)
        if taps < 3 or taps % 2 == 0:
            raise ValueError("taps must be an odd integer >= 3")
        divisor = math.gcd(source_rate, target_rate)
        self.ratio = ResampleRatio(target_rate // divisor, source_rate // divisor)
        self.source_rate = source_rate
        self.target_rate = target_rate
        self.taps = taps
        self.half = taps // 2
        self.cutoff = min(1.0, self.ratio.numerator / self.ratio.denominator)
        self._cache_start = 0
        self._cache = np.empty((0, 0), dtype=np.float64)
        self._offsets = np.arange(-self.half, self.half + 1, dtype=np.float64)

    def reset(self) -> None:
        self._cache_start = 0
        self._cache = np.empty((0, 0), dtype=np.float64)

    @property
    def cached_frames(self) -> int:
        return int(self._cache.shape[0])

    def _read_native(self, reader: NativeReader, start: int, count: int) -> np.ndarray:
        if count <= 0:
            return np.empty((0, 0), dtype=np.float64)
        end = start + count
        cache = self._cache
        cache_end = self._cache_start + cache.shape[0]
        if cache.size and start >= self._cache_start and end <= cache_end:
            offset = start - self._cache_start
            return cache[offset : offset + count]

        if cache.size and start < cache_end and end > cache_end:
            left = cache[start - self._cache_start :]
            right = np.asarray(reader(cache_end, end - cache_end), dtype=np.float64)
            data = np.concatenate((left, right), axis=0)
        else:
            data = np.asarray(reader(start, count), dtype=np.float64)
        if data.ndim == 1:
            data = data[:, None]
        if data.shape[0] != count:
            raise RuntimeError(
                f"native reader returned {data.shape[0]} frames, expected {count}"
            )
        self._cache_start = start
        self._cache = data
        return data

    def _weights(self, positions: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        indices = (
            np.floor(positions).astype(np.int64)[:, None]
            + self._offsets.astype(np.int64)[None, :]
        )
        distance = indices.astype(np.float64) - positions[:, None]
        kernel = self.cutoff * np.sinc(self.cutoff * distance)
        window = 0.5 + 0.5 * np.cos(np.pi * distance / (self.half + 1.0))
        window = np.where(np.abs(distance) < self.half + 1.0, window, 0.0)
        weights = kernel * window
        totals = np.sum(weights, axis=1, keepdims=True)
        weights = np.divide(weights, totals, out=np.zeros_like(weights), where=totals != 0)
        return indices, weights

    def render(self, reader: NativeReader, start: int, count: int) -> np.ndarray:
        """Pull ``count`` target-rate frames starting at absolute target frame."""
        if start < 0 or count < 0:
            raise ValueError("start and count must be non-negative")
        if count == 0:
            return np.empty((0, 0), dtype=np.float64)
        positions = (
            np.arange(start, start + count, dtype=np.float64)
            * self.ratio.denominator
            / self.ratio.numerator
        )
        indices, weights = self._weights(positions)
        native_start = int(indices[0, 0])
        native_end = int(indices[-1, -1]) + 1
        native = self._read_native(reader, native_start, native_end - native_start)
        gathered = native[indices - native_start]
        if gathered.ndim == 2:
            gathered = gathered[:, :, None]
        output = np.sum(gathered * weights[:, :, None], axis=1)
        return np.asarray(output, dtype=np.float64)

