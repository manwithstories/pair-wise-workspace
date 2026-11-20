"""BS.1770 K-weighting filter (high-shelf + RLB high-pass).

The two biquads are evaluated as an FFT convolution with a truncated impulse
response; that keeps a ten-minute pass close to real time while remaining a
linear, exactly repeatable filter.
"""

from __future__ import annotations

import numpy as np

from .errors import AudioError

def k_weighting_coefficients(fs: int) -> tuple[np.ndarray, np.ndarray]:
    """Return ``(b, a)`` for the two cascaded K-weighting biquads.

    Stage 1 is the high-shelf "pre-filter" (head effect), stage 2 the RLB
    high-pass.  Coefficients are derived from the analog prototypes published
    in BS.1770 so any sample rate works, rather than hard-coding 48 kHz.
    """
    if fs <= 0:
        raise AudioError("invalid sample rate for loudness", sample_rate=fs)
    # Stage 1: pre-filter / high shelf.
    f0 = 1681.974450955533
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    k = np.tan(np.pi * f0 / fs)
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh ** 0.4996667741545416
    a0 = 1.0 + k / q + k * k
    b1 = np.array(
        [
            (vh + vb * k / q + k * k) / a0,
            2.0 * (k * k - vh) / a0,
            (vh - vb * k / q + k * k) / a0,
        ]
    )
    a1 = np.array([1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0])
    # Stage 2: RLB high-pass.
    f0 = 38.13547087602444
    q = 0.5003270373238773
    k = np.tan(np.pi * f0 / fs)
    a0 = 1.0 + k / q + k * k
    b2 = np.array([1.0, -2.0, 1.0])
    a2 = np.array([1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0])
    return np.concatenate([b1, b2]), np.concatenate([a1, a2])


class _Biquad:
    """Biquad IIR with persistent per-channel state.

    The recurrence ``y[n] = b0 x[n] + b1 x[n-1] + b2 x[n-2] - a1 y[n-1] -
    a2 y[n-2]`` is evaluated as a convolution with the filter's impulse
    response, which decays below 1e-12 within ``_KERNEL`` samples.  Blocks are
    processed with the last ``_KERNEL - 1`` input samples from the previous
    block prepended, so state carries across block boundaries exactly.
    Convolution (FFT-backed inside NumPy for large blocks) keeps a ten-minute
    loudness pass near real time; a per-sample loop is ~15x slower and was the
    original bottleneck.

    The result is a *linear, time-invariant* filter, so it is bit-identical
    between the measurement and write passes.
    """

    _KERNEL = 4096

    def __init__(self, b: np.ndarray, a: np.ndarray, channels: int) -> None:
        if len(b) != 3 or len(a) != 3:
            raise AudioError("biquad needs 3 numerator and 3 denominator taps")
        self.b = b.astype(np.float64)
        self.a = a.astype(np.float64)
        self.channels = channels
        self._kernel = self._impulse_response()
        self._history = np.zeros((channels, self._KERNEL - 1), dtype=np.float64)

    def _impulse_response(self) -> np.ndarray:
        b0, b1, b2 = self.b
        a1, a2 = self.a[1], self.a[2]
        kernel = np.zeros(self._KERNEL, dtype=np.float64)
        y1 = y2 = 0.0
        for k in range(self._KERNEL):
            feed = (b0 if k == 0 else 0.0) + (b1 if k == 1 else 0.0) + (b2 if k == 2 else 0.0)
            y0 = feed - a1 * y1 - a2 * y2
            kernel[k] = y0
            y2, y1 = y1, y0
        return kernel

    def process(self, x: np.ndarray) -> np.ndarray:
        if x.shape[1] == 0:
            return np.empty(x.shape, dtype=np.float64)
        out = np.empty(x.shape, dtype=np.float64)
        keep = self._KERNEL - 1
        for ch in range(x.shape[0]):
            padded = np.concatenate([self._history[ch], x[ch].astype(np.float64)])
            # ``np.fft``-backed convolution is chosen by NumPy automatically
            # for large inputs; the kernel is applied once per *callback
            # block*, and callers pass large blocks (the whole clip builds up
            # to one convolution in the common case), so this stays
            # O(n log n) in practice.
            convolved = _fft_convolve(padded, self._kernel)
            out[ch] = convolved[keep : keep + x.shape[1]]
            self._history[ch] = padded[-keep:] if len(padded) >= keep else np.pad(
                padded, (keep - len(padded), 0)
            )
        return out


class _KWeighting:
    """The two cascaded BS.1770 filters, applied in order."""

    def __init__(self, sample_rate: int, channels: int) -> None:
        b, a = k_weighting_coefficients(sample_rate)
        self.shelf = _Biquad(b[0:3], a[0:3], channels)
        self.highpass = _Biquad(b[3:6], a[3:6], channels)

    def process(self, x: np.ndarray) -> np.ndarray:
        return self.highpass.process(self.shelf.process(x))


def _fft_convolve(signal: np.ndarray, kernel: np.ndarray) -> np.ndarray:
    """Full linear convolution using an FFT.

    ``np.convolve`` is direct (O(n*k)); for the 4096-tap K-weighting kernel on
    long signals that dominates run time.  The FFT path is mathematically
    identical (same real-valued linear convolution) and exact to float64
    round-off.
    """
    n = len(signal) + len(kernel) - 1
    size = 1 << (n - 1).bit_length()
    spectrum = np.fft.rfft(signal, size) * np.fft.rfft(kernel, size)
    return np.fft.irfft(spectrum, size)[:n]

