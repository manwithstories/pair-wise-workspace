"""Block FFT convolution with overlap-add — the single shared convolution core.

Both render paths (offline whole-array and streaming block-by-block) drive
this class and nothing else performs convolution. The implementation is a
textbook overlap-add built directly on NumPy FFT primitives:

  * the IR is zero-padded to a power-of-two FFT size ``K >= B + M - 1`` so
    circular wrap can never alias a block's tail into its head;
  * each input block is FFT'd, multiplied by the pre-computed IR spectrum
    and inverse-FFT'd;
  * every block result is summed into a running overlap buffer and the first
    ``B`` samples (fewer for the final partial block) are emitted;
  * ``flush`` emits the remaining ``M - 1``-sample decay tail. No silent
    truncation is possible: the emitted length is always N + M - 1.

Only ``numpy.fft`` routines are used — never ``scipy.signal.convolve`` or
any other ready-made convolution implementation.
"""
from __future__ import annotations

import numpy as np


def ensure_2d(a: np.ndarray, name: str) -> np.ndarray:
    """Promote a 1-D mono array to column-vector ``(n, 1)``; reject others."""
    if a.ndim == 1:
        return a[:, None]
    if a.ndim == 2:
        return a
    raise ValueError(f"{name} must be 1-D or 2-D, got {a.ndim} dimensions")


class BlockConvolver:
    """Stateful overlap-add convolver; one instance per render."""

    def __init__(self, ir: np.ndarray, block_size: int):
        if block_size <= 0:
            raise ValueError("block_size must be positive")
        ir2 = ensure_2d(ir, "ir").astype(np.float64, copy=False)
        self.m, self.channels = ir2.shape
        if self.m == 0:
            raise ValueError(
                "zero-length impulse response: convolution is undefined"
            )
        self.block_size = block_size
        # Smallest power of two holding a full block + full IR without wrap.
        needed = block_size + self.m - 1
        self.fft_size = 1 << (needed - 1).bit_length()

        # Pre-computed IR spectrum with the 1/K inverse-FFT factor folded in,
        # so multiplying by K after np.fft.ifft restores unscaled samples
        # (np.fft.ifft always applies its own 1/K normalisation).
        self._spectrum = np.fft.fft(ir2, n=self.fft_size, axis=0) / self.fft_size

        # Overlap buffer always covers the positions AFTER the last emitted
        # sample; it is trimmed back to that window on every block.
        self._overlap = np.zeros((0, self.channels), dtype=np.float64)
        self._emitted = 0
        self._flushed = False

    # ------------------------------------------------------------------
    def process_block(self, block: np.ndarray) -> np.ndarray:
        """Convolve one (possibly partial) input block; emit B' output rows.

        A full block emits exactly ``block_size`` rows; the final partial
        block emits its own length. The residual IR tail stays buffered for
        ``flush``.
        """
        if self._flushed:
            raise RuntimeError("process_block called after flush")
        block2 = ensure_2d(block, "block").astype(np.float64, copy=False)
        if block2.shape[1] != self.channels:
            raise ValueError(
                f"block has {block2.shape[1]} channel(s), convolver was "
                f"constructed with {self.channels} channel(s)"
            )
        if block2.shape[0] == 0:
            return np.zeros((0, self.channels), dtype=np.float64)
        if block2.shape[0] > self.block_size:
            raise ValueError(
                f"block length {block2.shape[0]} exceeds block_size "
                f"{self.block_size}; callers must split the stream into "
                "blocks no larger than the configured block size"
            )

        spectrum = np.fft.fft(block2, n=self.fft_size, axis=0)
        product = spectrum * self._spectrum
        # Cancel np.fft.ifft's built-in 1/K (the factor lives in _spectrum).
        result = self.fft_size * np.real(np.fft.ifft(product, axis=0))

        # Place the new result on the absolute-position axis, then fold the
        # previous overlap in. Positions are derived, never guessed, so this
        # stays correct when the IR is shorter than one block as well. Only
        # the valid linear-convolution span (L + M - 1) of the FFT result is
        # real signal; the rest is zero-pad and must never be placed.
        valid_len = block2.shape[0] + self.m - 1
        start = self._emitted
        end = start + valid_len
        overlap_end = self._emitted + self._overlap.shape[0]

        merged = np.zeros((end - start, self.channels), dtype=np.float64)
        merged[:valid_len, :] += result[:valid_len, :]
        if overlap_end > start:
            # The overlap window always starts at the current emit position.
            span = min(self._overlap.shape[0], end - start)
            merged[:span, :] += self._overlap[:span, :]

        # Emit at most one block, but never more than the valid span: for a
        # short (n < B) input the first block's linear convolution itself is
        # shorter than B. The remainder is the IR tail, handled by flush.
        emit_n = min(block2.shape[0], valid_len)
        emitted = merged[:emit_n, :].copy()
        self._overlap = merged[emit_n:, :].copy()
        self._emitted += emit_n
        return emitted

    # ------------------------------------------------------------------
    def flush(self) -> np.ndarray:
        """Emit the buffered decay tail.

        For a non-empty input this is exactly the ``M - 1``-sample IR tail.
        An empty input still has a mathematically defined convolution of
        length ``M - 1`` (all zeros), so flush guarantees that length even
        when no block was ever processed.
        """
        if self._flushed:
            raise RuntimeError("flush called twice")
        self._flushed = True
        tail = self._overlap
        if tail.shape[0] < self.m - 1:
            pad = np.zeros(
                (self.m - 1 - tail.shape[0], self.channels), dtype=np.float64
            )
            tail = np.concatenate([pad, tail], axis=0) if tail.shape[0] else pad
        self._overlap = np.zeros((0, self.channels), dtype=np.float64)
        return tail.copy()

    # ------------------------------------------------------------------
    @property
    def emitted_samples(self) -> int:
        return self._emitted


def convolve_streaming(ir: np.ndarray, blocks, block_size: int):
    """Yield output chunks for an iterable of input blocks via one core.

    Generator form: the streaming path consumes real block boundaries one
    at a time; the offline path feeds pre-sliced chunks through the same
    method, guaranteeing identical arithmetic.
    """
    core = BlockConvolver(ir, block_size)
    for block in blocks:
        yield core.process_block(block)
    yield core.flush()
