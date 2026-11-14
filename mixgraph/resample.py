"""Streaming rational-ratio resampler with a polyphase anti-alias filter."""
from __future__ import annotations

from math import gcd

import numpy as np
from numpy.lib.stride_tricks import sliding_window_view


class ResampleError(ValueError):
    """Raised for invalid resampling configuration."""


class PolyphaseResampler:
    """Resample float64 audio while preserving all state across chunks.

    The output sample at index ``n`` is evaluated at input time
    ``n * output_rate / input_rate`` using a windowed-sinc fractional-delay
    filter.  Coefficients are normalized per phase for unity DC gain.
    """

    def __init__(
        self,
        input_rate: int,
        output_rate: int,
        channels: int = 1,
        total_input_frames: int | None = None,
        half_width: int = 32,
    ):
        if input_rate <= 0 or output_rate <= 0:
            raise ResampleError("resample rates must be positive")
        if channels not in (1, 2):
            raise ResampleError("resample channels must be 1 or 2")
        common = gcd(int(input_rate), int(output_rate))
        self.input_rate = int(input_rate)
        self.output_rate = int(output_rate)
        self.up = self.output_rate // common
        self.down = self.input_rate // common
        self.channels = int(channels)
        self.half_width = int(half_width)
        if self.half_width < 4:
            raise ResampleError("resampler half_width must be at least 4")
        self.total_input_frames = (
            None if total_input_frames is None else int(total_input_frames)
        )
        if self.total_input_frames is not None and self.total_input_frames < 0:
            raise ResampleError("total input frames must not be negative")
        self.output_frames = (
            0
            if self.total_input_frames is None
            else (self.total_input_frames * self.up + self.down // 2) // self.down
        )
        self._buffer = np.zeros((self.channels, self.half_width), dtype=np.float64)
        self._base = -self.half_width
        self._total_in = 0
        self._next_out = 0
        self._finished = False
        self._coefficient_cache: dict[int, np.ndarray] = {}

    @property
    def total_input(self) -> int:
        return self._total_in

    @property
    def next_output(self) -> int:
        return self._next_out

    def _coefficients(self, phase: int) -> np.ndarray:
        cached = self._coefficient_cache.get(phase)
        if cached is not None:
            return cached
        half = self.half_width
        offsets = np.arange(-half, half + 1, dtype=np.float64)
        fractional = phase / self.up
        cutoff = 0.5 * min(1.0, self.output_rate / self.input_rate) * 0.94
        distance = offsets - fractional
        sinc_values = 2.0 * cutoff * np.sinc(2.0 * cutoff * distance)
        window = np.where(
            np.abs(distance) < half,
            0.5 + 0.5 * np.cos(np.pi * distance / half),
            0.0,
        )
        coefficients = sinc_values * window
        total = float(coefficients.sum())
        if abs(total) < 1e-15:
            coefficients = np.zeros_like(coefficients)
            coefficients[half] = 1.0
        else:
            coefficients /= total
        self._coefficient_cache[phase] = coefficients
        return coefficients

    def process(self, block: np.ndarray) -> np.ndarray:
        if self._finished:
            raise ResampleError("resampler already finished")
        audio = np.asarray(block, dtype=np.float64)
        if audio.ndim == 1:
            audio = audio[None, :]
        if audio.shape[0] != self.channels:
            raise ResampleError(
                f"expected {self.channels} channel(s), got {audio.shape[0]}"
            )
        if self.total_input_frames is not None:
            if self._total_in + audio.shape[1] > self.total_input_frames:
                raise ResampleError("input exceeds declared total_input_frames")
        if audio.shape[1]:
            self._buffer = np.concatenate((self._buffer, audio), axis=1)
        self._total_in += audio.shape[1]
        safe = self._total_in - self.half_width
        allowed = 0 if safe <= 0 else (safe * self.up + self.down - 1) // self.down
        target = min(allowed, self.output_frames)
        return self._emit_until(target)

    def _emit_until(self, target: int) -> np.ndarray:
        if target <= self._next_out:
            return np.zeros((self.channels, 0), dtype=np.float64)
        indices = np.arange(self._next_out, target, dtype=np.int64)
        numerators = indices * self.down
        starts_input = numerators // self.up
        phases = numerators % self.up
        window_starts = starts_input - self._base - self.half_width
        windows = sliding_window_view(
            self._buffer, 2 * self.half_width + 1, axis=1
        )
        selected = windows[:, window_starts, :]
        coefficients = np.stack([self._coefficients(int(p)) for p in phases])
        output = np.einsum("ckw,kw->ck", selected, coefficients, optimize=True)
        self._next_out = target
        next_start = (self._next_out * self.down // self.up) - self.half_width
        trim = max(0, next_start - self._base)
        if trim:
            self._buffer = self._buffer[:, trim:]
            self._base += trim
        return output

    def finish(self) -> np.ndarray:
        if self._finished:
            return np.zeros((self.channels, 0), dtype=np.float64)
        self._finished = True
        if self._next_out >= self.output_frames:
            return np.zeros((self.channels, 0), dtype=np.float64)
        tail = np.zeros((self.channels, self.half_width), dtype=np.float64)
        self._buffer = np.concatenate((self._buffer, tail), axis=1)
        return self._emit_until(self.output_frames)
