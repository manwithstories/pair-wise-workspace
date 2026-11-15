"""Streaming BS.1770-4 K-weighting and gated integrated loudness."""

from __future__ import annotations

from functools import lru_cache
import math

import numpy as np


LOUDNESS_OFFSET = -0.691
ABSOLUTE_GATE_LUFS = -70.0
RELATIVE_GATE_LU = -8.0
DB_FLOOR = -200.0


def amplitude_to_db(value: float) -> float:
    if not math.isfinite(value) or value <= 0.0:
        return DB_FLOOR
    return max(DB_FLOOR, 20.0 * math.log10(value))


def power_to_lufs(power: float) -> float:
    if not math.isfinite(power) or power <= 0.0:
        return DB_FLOOR
    return max(DB_FLOOR, LOUDNESS_OFFSET + 10.0 * math.log10(power))


@lru_cache(maxsize=32)
def k_weighting_coefficients(rate: int) -> tuple[np.ndarray, ...]:
    """Return normalized shelf and RLB biquads adapted to ``rate``."""

    shelf_f = 1681.974450955533
    shelf_gain = 3.999843853973347
    shelf_q = 0.7071752369554196
    k = math.tan(math.pi * shelf_f / rate)
    vh = 10.0 ** (shelf_gain / 20.0)
    vb = vh**0.4996667741545416
    a0 = 1.0 + k / shelf_q + k * k
    shelf_b = np.array(
        [
            (vh + vb * k / shelf_q + k * k) / a0,
            2.0 * (k * k - vh) / a0,
            (vh - vb * k / shelf_q + k * k) / a0,
        ],
        dtype=np.float64,
    )
    shelf_a = np.array(
        [1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / shelf_q + k * k) / a0],
        dtype=np.float64,
    )

    highpass_f = 38.13547087602444
    highpass_q = 0.5003270373238773
    k = math.tan(math.pi * highpass_f / rate)
    a0 = 1.0 + k / highpass_q + k * k
    highpass_b = np.array([1.0, -2.0, 1.0], dtype=np.float64) / a0
    highpass_a = np.array(
        [
            1.0,
            2.0 * (k * k - 1.0) / a0,
            (1.0 - k / highpass_q + k * k) / a0,
        ],
        dtype=np.float64,
    )
    return shelf_b, shelf_a, highpass_b, highpass_a


class KWeightingFilter:
    def __init__(self, rate: int, channels: int) -> None:
        self.rate = int(rate)
        self.channels = int(channels)
        (
            self.shelf_b,
            self.shelf_a,
            self.highpass_b,
            self.highpass_a,
        ) = k_weighting_coefficients(self.rate)
        self._s1 = np.zeros((self.channels, 2), dtype=np.float64)
        self._s2 = np.zeros((self.channels, 2), dtype=np.float64)

    def process(self, block: np.ndarray) -> np.ndarray:
        data = np.asarray(block, dtype=np.float64)
        if data.ndim == 1:
            data = data[:, None]
        if data.shape[1] != self.channels:
            raise ValueError("channel count changed during loudness measurement")
        if len(data) == 0:
            return np.empty((0, self.channels), dtype=np.float64)
        output = np.empty_like(data)
        b10, b11, b12 = self.shelf_b
        _, a11, a12 = self.shelf_a
        b20, b21, b22 = self.highpass_b
        _, a21, a22 = self.highpass_a
        z1 = self._s1
        z2 = self._s2
        for index in range(len(data)):
            x = data[index]
            y1 = b10 * x + z1[:, 0]
            z1[:, 0] = b11 * x - a11 * y1 + z1[:, 1]
            z1[:, 1] = b12 * x - a12 * y1
            y2 = b20 * y1 + z2[:, 0]
            z2[:, 0] = b21 * y1 - a21 * y2 + z2[:, 1]
            z2[:, 1] = b22 * y1 - a22 * y2
            output[index] = y2
        return output


class LoudnessMeter:
    """Integrated loudness from 400 ms blocks with 75% overlap."""

    def __init__(self, rate: int, channels: int = 2) -> None:
        self.rate = int(rate)
        self.channels = int(channels)
        self.window = max(1, int(round(self.rate * 0.400)))
        self.hop = max(1, int(round(self.rate * 0.100)))
        self._filter = KWeightingFilter(self.rate, self.channels)
        self._pending = np.empty((0, self.channels), dtype=np.float64)
        self._energies: list[float] = []
        self._frames = 0
        self._result: float | None = None

    def _consume(self, filtered: np.ndarray) -> None:
        if len(filtered):
            self._pending = np.concatenate((self._pending, filtered), axis=0)
        while len(self._pending) >= self.window:
            window = self._pending[: self.window]
            energy = float(np.mean(np.square(window), axis=0).sum())
            self._energies.append(energy)
            self._pending = self._pending[self.hop :].copy()

    def process(self, block: np.ndarray) -> None:
        if self._result is not None:
            raise RuntimeError("loudness meter already finalized")
        data = np.asarray(block, dtype=np.float64)
        if data.ndim == 1:
            data = data[:, None]
        self._frames += len(data)
        self._consume(self._filter.process(data))

    def finalize(self) -> float:
        if self._result is not None:
            return self._result
        if not self._energies and len(self._pending):
            energy = float(np.mean(np.square(self._pending), axis=0).sum())
            self._energies.append(energy)
        if not self._energies:
            self._result = DB_FLOOR
            return self._result

        powers = np.asarray(self._energies, dtype=np.float64)
        loudness = np.asarray([power_to_lufs(value) for value in powers])
        above_absolute = powers[loudness >= ABSOLUTE_GATE_LUFS]
        if not len(above_absolute):
            self._result = DB_FLOOR
            return self._result
        relative_gate = float(np.mean(above_absolute)) * 10.0 ** (
            RELATIVE_GATE_LU / 10.0
        )
        gated = powers[(powers >= relative_gate) & (loudness >= ABSOLUTE_GATE_LUFS)]
        self._result = power_to_lufs(float(np.mean(gated)))
        return self._result


class RmsMeter:
    def __init__(self, channels: int) -> None:
        self.channels = int(channels)
        self._sum_square = 0.0
        self._samples = 0
        self._result: float | None = None

    def process(self, block: np.ndarray) -> None:
        data = np.asarray(block, dtype=np.float64)
        if data.ndim == 1:
            data = data[:, None]
        self._sum_square += float(np.sum(np.square(data), dtype=np.float64))
        self._samples += int(data.size)

    def finalize(self) -> float:
        if self._result is None:
            if not self._samples or self._sum_square <= 0.0:
                self._result = DB_FLOOR
            else:
                rms = math.sqrt(self._sum_square / self._samples)
                self._result = amplitude_to_db(rms)
        return self._result


def integrated_loudness(
    samples: np.ndarray, rate: int, block_size: int = 4096
) -> float:
    data = np.asarray(samples, dtype=np.float64)
    channels = 1 if data.ndim == 1 else data.shape[1]
    meter = LoudnessMeter(rate, channels)
    for start in range(0, len(data), block_size):
        meter.process(data[start : start + block_size])
    return meter.finalize()
