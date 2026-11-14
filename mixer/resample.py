"""Windowed-sinc rational polyphase resampler with streaming history."""

from __future__ import annotations

import math

import numpy as np

from .errors import RenderError


class SincResampler:
    """Resample channel-first blocks while retaining filter look-ahead/history."""

    def __init__(
        self,
        in_rate: int,
        out_rate: int,
        channels: int,
        *,
        taps: int = 32,
        rolloff: float = 0.95,
    ) -> None:
        if in_rate <= 0 or out_rate <= 0 or channels <= 0:
            raise RenderError("resampler rates and channels must be positive")
        if taps < 8 or taps % 2:
            raise RenderError("resampler taps must be an even integer >= 8")
        divisor = math.gcd(in_rate, out_rate)
        self.in_rate = in_rate
        self.out_rate = out_rate
        self.channels = channels
        self.taps = taps
        self.pre = taps // 2
        self.lookahead = taps - self.pre - 1
        self.numerator = in_rate // divisor
        self.denominator = out_rate // divisor
        self.cutoff = min(1.0, out_rate / in_rate) * rolloff
        self.kernel = self._build_kernel()
        self._buffer = np.zeros((channels, 0), dtype=np.float32)
        self._buffer_start = 0
        self._total_in = 0
        self._next_out = 0
        self._target_out: int | None = None
        self._eof = False

    def _build_kernel(self) -> np.ndarray:
        phase_count = self.denominator
        k = np.arange(self.taps, dtype=np.float64)
        phase = np.arange(phase_count, dtype=np.float64)[:, None] / phase_count
        offsets = k[None, :] - self.pre - phase
        sinc = np.sinc(offsets * self.cutoff)
        window = np.kaiser(self.taps, 8.0)[None, :]
        kernel = sinc * window
        sums = kernel.sum(axis=1, keepdims=True)
        kernel = np.divide(kernel, sums, out=np.zeros_like(kernel), where=np.abs(sums) > 1e-15)
        return kernel.astype(np.float32)

    def set_target(self, target_out: int) -> None:
        if target_out < 0:
            raise RenderError("resampler target length must be non-negative")
        if self._next_out > target_out:
            raise RenderError("resampler target length was reduced after output started")
        self._target_out = int(target_out)

    @property
    def produced(self) -> int:
        return self._next_out

    @property
    def total_in(self) -> int:
        return self._total_in

    def push(self, frames: np.ndarray) -> np.ndarray:
        block = self._normalize(frames)
        if self._eof:
            if block.shape[1]:
                raise RenderError("cannot push audio after resampler EOF")
            return self._empty()
        if block.shape[1]:
            self._buffer = np.concatenate((self._buffer, block), axis=1)
            self._total_in += block.shape[1]
        return self._emit(padded=False)

    def flush(self, max_frames: int | None = None) -> np.ndarray:
        self._eof = True
        if max_frames is not None and max_frames <= 0:
            return self._empty()
        return self._emit(padded=True, limit=max_frames)

    def _normalize(self, frames: np.ndarray) -> np.ndarray:
        block = np.asarray(frames, dtype=np.float32)
        if block.ndim == 1:
            block = block[None, :]
        if block.ndim != 2 or block.shape[0] != self.channels:
            raise RenderError(
                "resampler block has an unexpected channel layout",
                details={"expected_channels": self.channels, "shape": list(block.shape)},
            )
        return block

    def _empty(self) -> np.ndarray:
        return np.zeros((self.channels, 0), dtype=np.float32)

    def _emit(self, *, padded: bool, limit: int | None = None) -> np.ndarray:
        if self._target_out is None:
            raise RenderError("resampler target length was not configured")
        if self._next_out >= self._target_out:
            return self._empty()
        estimated_end = self._target_out
        if limit is not None:
            estimated_end = min(estimated_end, self._next_out + limit)
        if not padded:
            available_base = self._total_in - 1 - self.lookahead
            if available_base < 0:
                return self._empty()
            available_end = min(
                self._target_out,
                ((available_base + 1) * self.denominator) // self.numerator,
            )
            estimated_end = min(estimated_end, available_end)
        if estimated_end <= self._next_out:
            return self._empty()

        count = estimated_end - self._next_out
        output_index = np.arange(self._next_out, estimated_end, dtype=np.int64)
        numerator = output_index * self.numerator
        base = numerator // self.denominator
        phase = (numerator % self.denominator).astype(np.int64)
        if not padded:
            valid = base + self.lookahead < self._total_in
            if not np.all(valid):
                count = int(np.argmin(valid))
                if count == 0:
                    return self._empty()
                output_index = output_index[:count]
                base = base[:count]
                phase = phase[:count]
        if count <= 0:
            return self._empty()

        input_index = base[:, None] - self.pre + np.arange(self.taps, dtype=np.int64)[None, :]
        relative = input_index - self._buffer_start
        valid_mask = (relative >= 0) & (relative < self._buffer.shape[1]) & (input_index >= 0)
        valid_mask &= input_index < self._total_in
        safe_relative = np.clip(relative, 0, max(self._buffer.shape[1] - 1, 0))
        if self._buffer.shape[1] == 0:
            samples = np.zeros((self.channels, count, self.taps), dtype=np.float32)
        else:
            samples = self._buffer[:, safe_relative]
            samples *= valid_mask[None, :, :]
        weights = self.kernel[phase]
        output = np.sum(samples * weights[None, :, :], axis=2, dtype=np.float32)

        self._next_out += count
        next_base = (self._next_out * self.numerator) // self.denominator
        keep_from = max(0, next_base - self.pre)
        if keep_from > self._buffer_start:
            drop = min(keep_from - self._buffer_start, self._buffer.shape[1])
            self._buffer = self._buffer[:, drop:]
            self._buffer_start += drop
        return output
