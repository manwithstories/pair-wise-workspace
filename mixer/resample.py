"""Rational, block-safe resampling implemented with NumPy only."""

from __future__ import annotations

import math
from fractions import Fraction

import numpy as np


MAX_DENOMINATOR = 1_000_000


def rational_ratio(source_sr: int, target_sr: int) -> Fraction:
    """Return the reduced target/source ratio, approximated when necessary."""

    if not isinstance(source_sr, (int, np.integer)) or source_sr <= 0:
        raise ValueError("source sample rate must be a positive integer")
    if not isinstance(target_sr, (int, np.integer)) or target_sr <= 0:
        raise ValueError("target sample rate must be a positive integer")
    return Fraction(int(target_sr), int(source_sr)).limit_denominator(
        MAX_DENOMINATOR
    )


reduce_ratio = rational_ratio
resample_ratio = rational_ratio


def resampled_length(input_frames: int, up: int, down: int) -> int:
    """Round-half-up the ideal output length for a positive frame count."""

    if input_frames <= 0:
        return 0
    if up <= 0 or down <= 0:
        raise ValueError("resampling factors must be positive")
    return (2 * int(input_frames) * int(up) + int(down)) // (2 * int(down))


def design_lowpass(up: int, down: int, taps: int | None = None) -> np.ndarray:
    """Design a windowed-sinc lowpass for the upsampled-rate convolution."""

    if up <= 0 or down <= 0:
        raise ValueError("resampling factors must be positive")
    scale = max(int(up), int(down))
    taps = int(taps or max(64, 16 * scale))
    if taps < 8:
        raise ValueError("filter must have at least 8 taps")
    cutoff = 0.5 * min(1.0, float(down) / float(up)) / float(up)
    positions = np.arange(taps, dtype=np.float64) - (taps - 1) / 2.0
    kernel = 2.0 * cutoff * np.sinc(2.0 * cutoff * positions)
    kernel *= np.kaiser(taps, 8.6)
    total = float(kernel.sum())
    if not math.isfinite(total) or abs(total) < 1e-15:
        raise ValueError("failed to design resampling filter")
    return kernel / total


def _as_audio(data: np.ndarray, channels: int | None = None) -> np.ndarray:
    array = np.asarray(data, dtype=np.float64)
    if array.ndim == 1:
        array = array[:, None]
    if array.ndim != 2:
        raise ValueError("audio data must be one- or two-dimensional")
    if channels is not None and array.shape[1] != channels:
        raise ValueError("channel count changed during resampling")
    return array


class RationalResampler:
    """Stateful L/M resampler whose output is invariant to chunk boundaries.

    Output sample ``n`` is computed from a causal polyphase filter using the
    absolute upsampled index ``n * M``. ``process`` emits the rounded ideal
    number of output frames for all input received so far; ``flush`` therefore
    adds no further samples but is provided for a conventional stream API.
    """

    def __init__(
        self,
        source_sr: int,
        target_sr: int,
        taps: int | None = None,
        channels: int | None = None,
    ) -> None:
        self.ratio = rational_ratio(source_sr, target_sr)
        self.up = self.ratio.numerator
        self.down = self.ratio.denominator
        self.source_sr = int(source_sr)
        self.target_sr = int(target_sr)
        self.channels = channels
        self.kernel = design_lowpass(self.up, self.down, taps=taps)
        self._buffer = np.empty((0, channels or 0), dtype=np.float64)
        self._buffer_start = 0
        self._total_input = 0
        self._next_output = 0

    @property
    def taps(self) -> int:
        return int(self.kernel.size)

    @property
    def total_input(self) -> int:
        return self._total_input

    @property
    def total_output(self) -> int:
        return self._next_output

    def reset(self) -> None:
        channels = self.channels or 0
        self._buffer = np.empty((0, channels), dtype=np.float64)
        self._buffer_start = 0
        self._total_input = 0
        self._next_output = 0

    def _compute(self, start_out: int, end_out: int) -> np.ndarray:
        count = end_out - start_out
        if count <= 0:
            channels = self.channels or 0
            return np.empty((0, channels), dtype=np.float64)
        out_numbers = np.arange(start_out, end_out, dtype=np.int64)
        positions = out_numbers * self.down
        remainder = positions % self.up
        base_input = (positions - remainder) // self.up
        result = np.zeros((count, self.channels), dtype=np.float64)
        max_q = (self.taps - 1) // self.up + 1
        for q in range(max_q):
            kernel_index = remainder + q * self.up
            local_index = base_input - self._buffer_start - q
            valid = (
                (kernel_index < self.taps)
                & (local_index >= 0)
                & (local_index < self._buffer.shape[0])
            )
            if not np.any(valid):
                continue
            coeff = self.kernel[kernel_index[valid]]
            samples = self._buffer[local_index[valid]]
            result[valid] += coeff[:, None] * samples
        return result * float(self.up)

    def process(self, data: np.ndarray) -> np.ndarray:
        """Consume an input chunk and return all output frames now determined."""

        chunk = _as_audio(data, self.channels)
        if self.channels is None:
            self.channels = int(chunk.shape[1])
        if chunk.shape[1] and self.channels != chunk.shape[1]:
            raise ValueError("channel count changed during resampling")
        if chunk.shape[0]:
            self._buffer = np.concatenate((self._buffer, chunk), axis=0)
            if self._buffer.shape[1] == 0:
                self._buffer = np.empty((self._buffer.shape[0], self.channels))
            self._total_input += int(chunk.shape[0])
        target = resampled_length(self._total_input, self.up, self.down)
        result = self._compute(self._next_output, target)
        self._next_output = target
        keep_from = max(0, (self._next_output * self.down - (self.taps - 1)) // self.up)
        drop = max(0, keep_from - self._buffer_start)
        if drop > 0:
            self._buffer = self._buffer[drop:]
            self._buffer_start += drop
        return result

    def flush(self) -> np.ndarray:
        """Finish the stream; output was already rounded and emitted."""

        channels = self.channels or 0
        return np.empty((0, channels), dtype=np.float64)


def resample_array(data: np.ndarray, source_sr: int, target_sr: int) -> np.ndarray:
    """Convenience wrapper for whole-array resampling."""

    array = _as_audio(data)
    if source_sr == target_sr:
        return array
    converter = RationalResampler(source_sr, target_sr, channels=array.shape[1])
    return np.concatenate((converter.process(array), converter.flush()), axis=0)


Resampler = RationalResampler
resample = resample_array
output_length = resampled_length
