"""Streaming rational resampler with exact block-boundary continuity."""

from __future__ import annotations

from math import floor

import numpy as np

from .kernel import ResamplingKernel, design_kernel


def output_frame_count(frames: int, kernel: ResamplingKernel) -> int:
    return int(floor(frames * kernel.L / kernel.M + 0.5))


class Resampler:
    """Block resampler preserving FIR phase and full future-tap lookahead.

    Each output sample is a centered dot product over one polyphase row.  Input
    samples are retained until every output that can still need them has been
    emitted.  ``flush`` treats samples beyond EOF as zeros and stops at the
    rounded output length, so no group-delay tail leaks into the result.
    """

    def __init__(
        self,
        fs_in: int,
        fs_out: int,
        quality: str = "high",
    ) -> None:
        self.fs_in = int(fs_in)
        self.fs_out = int(fs_out)
        self.identity = self.fs_in == self.fs_out
        self.kernel = (
            None if self.identity else design_kernel(self.fs_in, self.fs_out, quality)
        )
        self._buffer = np.empty((0, 0), dtype=np.float64)
        self._base = 0
        self._channels: int | None = None
        self._one_d = False
        self._received = 0
        self._next_out = 0
        self._finalized = False
        self._output_length: int | None = None

    @property
    def fingerprint(self) -> str:
        if self.kernel is None:
            return "bypass"
        return self.kernel.fingerprint

    @property
    def output_length(self) -> int | None:
        return self._output_length

    def _prepare(self, block: np.ndarray) -> np.ndarray:
        data = np.asarray(block, dtype=np.float64)
        if data.ndim == 1:
            if self._channels not in (None, 1):
                raise ValueError("channel count changed between blocks")
            self._one_d = True
            data = data[:, None]
        elif data.ndim == 2:
            if self._channels not in (None, data.shape[1]):
                raise ValueError("channel count changed between blocks")
        else:
            raise ValueError("audio blocks must be 1-D or 2-D")
        if data.shape[0] and data.shape[1] == 0:
            raise ValueError("audio blocks must contain channels")
        self._channels = data.shape[1]
        return np.ascontiguousarray(data)

    def push(self, block: np.ndarray) -> np.ndarray:
        if self._finalized:
            raise RuntimeError("cannot push after flush")
        data = self._prepare(block)
        if self.identity:
            self._received += len(data)
            return data[:, 0].copy() if self._one_d else data.copy()

        if len(data):
            if not len(self._buffer):
                self._buffer = data.copy()
            else:
                self._buffer = np.concatenate((self._buffer, data), axis=0)
            self._received += len(data)
        return self._emit(final=False)

    def _safe_end(self) -> int:
        assert self.kernel is not None
        max_q = self._received - 1 - self.kernel.half_span
        if max_q < 0:
            return self._next_out
        limited = ((max_q + 1) * self.kernel.L + self.kernel.M - 1) // self.kernel.M
        return max(self._next_out, limited)

    def _emit(self, final: bool) -> np.ndarray:
        assert self.kernel is not None
        if final:
            end = output_frame_count(self._received, self.kernel)
        else:
            end = self._safe_end()
        if end <= self._next_out:
            return np.empty((0, self._channels or 0), dtype=np.float64)

        table = self.kernel.table
        offsets = self.kernel.offsets
        span = len(offsets)
        channels = self._channels or 1
        chunk = max(1, min(1024, 100_000 // max(1, span * channels)))
        offsets32 = offsets.astype(np.int32, copy=False)
        pieces: list[np.ndarray] = []
        for start in range(self._next_out, end, chunk):
            stop = min(end, start + chunk)
            n = np.arange(start, stop, dtype=np.int64)
            product = n * self.kernel.M
            q = product // self.kernel.L
            phase = product % self.kernel.L
            indices = q.astype(np.int32)[:, None] - offsets32[None, :]
            relative = indices - self._base
            valid = (indices >= 0) & (indices < self._received)
            clipped = np.clip(relative, 0, max(0, len(self._buffer) - 1))
            values = self._buffer[clipped]
            if not valid.all():
                values[~valid] = 0.0
            coeffs = table[phase]
            values *= coeffs[:, :, None]
            pieces.append(np.sum(values, axis=1))
        self._next_out = end
        self._trim()
        result = np.concatenate(pieces, axis=0)
        return result[:, 0] if self._one_d else result

    def _trim(self) -> None:
        assert self.kernel is not None
        q_next = (self._next_out * self.kernel.M) // self.kernel.L
        keep_from = max(0, q_next - self.kernel.half_span)
        cut = min(max(0, keep_from - self._base), len(self._buffer))
        if cut:
            self._buffer = self._buffer[cut:].copy()
            self._base += cut

    def flush(self) -> np.ndarray:
        if self._finalized:
            return np.empty((0, self._channels or 0), dtype=np.float64)
        self._finalized = True
        if self.identity:
            self._output_length = self._received
            return np.empty(0) if self._one_d else np.empty((0, self._channels or 0))
        result = self._emit(final=True)
        self._output_length = self._next_out
        self._buffer = np.empty((0, self._channels or 0), dtype=np.float64)
        return result


def resample_array(
    samples: np.ndarray,
    fs_in: int,
    fs_out: int,
    block_size: int = 4096,
    quality: str = "high",
) -> np.ndarray:
    """Convenience wrapper used by tests and single-file callers."""

    if block_size <= 0:
        raise ValueError("block_size must be positive")
    inputs = np.asarray(samples, dtype=np.float64)
    stream = Resampler(fs_in, fs_out, quality)
    pieces = [
        stream.push(inputs[start : start + block_size])
        for start in range(0, len(inputs), block_size)
    ]
    pieces.append(stream.flush())
    pieces = [piece for piece in pieces if len(piece)]
    if not pieces:
        return np.empty_like(inputs, dtype=np.float64)
    return np.concatenate(pieces, axis=0)
