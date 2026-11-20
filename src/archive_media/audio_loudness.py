"""Streaming BS.1770-4 loudness measurement and gain control.

The meter is fed block-by-block but keeps continuous IIR state, so measuring
the whole programme in chunks gives the identical result to one pass while
memory stays bounded by the 400 ms gating window.  Gain is computed once from
the full-programme measurement (the pipeline runs a measure pass first) and
then applied per chunk; a separate limiter honours the true-peak ceiling.

The final block may be shorter than requested: :class:`ChunkGainProcessor`
reports the number of samples actually written and the boundary error against
the expected output length, instead of padding or dropping samples silently.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field

import numpy as np

from .errors import Code, RecoveryError
from .timeline import rational

BLOCK_SECONDS = 0.4
HOP_SECONDS = 0.1
MIN_GAIN_DB = -60.0
MAX_GAIN_DB = 24.0


def _k_weighting(fs: int) -> tuple[tuple, tuple, tuple, tuple]:
    """Biquad coefficients for the BS.1770 K-weighting two-stage filter."""
    g, q, fc = 3.999843853973347, 0.7071752369554196, 1681.974450955533
    k = math.tan(math.pi * fc / fs)
    vh = 10.0 ** (g / 20.0)
    vb = vh**0.4996667741545416
    a0 = 1.0 + k / q + k * k
    b1 = ((vh + vb * k / q + k * k) / a0, 2.0 * (k * k - vh) / a0,
          (vh - vb * k / q + k * k) / a0)
    a1 = (1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0)
    q, fc = 0.5003270373238773, 38.13547087602444
    k = math.tan(math.pi * fc / fs)
    a0 = 1.0 + k / q + k * k
    b2 = (1.0, -2.0, 1.0)
    a2 = (1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0)
    return b1, a1, b2, a2


def _gated_loudness(powers: list[float]) -> float | None:
    if not powers:
        return None
    values = np.asarray(powers, dtype=np.float64)
    z = -0.691 + 10.0 * np.log10(np.maximum(values, 1e-12))
    absolute = z > -70.0
    if not absolute.any():
        return None
    relative = -0.691 + 10.0 * math.log10(values[absolute].mean()) - 10.0
    gated = absolute & (z > relative)
    if not gated.any():
        gated = absolute
    return float(-0.691 + 10.0 * math.log10(values[gated].mean()))


class LoudnessMeter:
    """Streaming K-weighted meter with per-channel continuous filter state."""

    def __init__(self, rate: int, channels: int) -> None:
        self.rate = int(rate)
        self.channels = int(channels)
        self._steps = _k_weighting(self.rate)
        self._block = max(1, int(round(BLOCK_SECONDS * self.rate)))
        self._hop = max(1, int(round(HOP_SECONDS * self.rate)))
        self.powers: list[float] = []
        self.peak = 0.0
        self.sample_count = 0
        self.nonfinite_samples = 0
        self._state = [[0.0] * 6 for _ in range(self.channels)]
        self._energy = np.zeros(0, dtype=np.float64)

    def _filter_stage(self, x: np.ndarray, state: list[float]) -> np.ndarray:
        b1, a1, b2, a2 = self._steps
        x1, x2, w1, w2, y1, y2 = state
        y = np.empty_like(x)
        for i in range(x.shape[0]):
            xi = float(x[i])
            w = b1[0] * xi + b1[1] * x1 + b1[2] * x2 - a1[1] * w1 - a1[2] * w2
            x2, x1 = x1, xi
            out = b2[0] * w + b2[1] * w1 + b2[2] * w2 - a2[1] * y1 - a2[2] * y2
            w2, w1 = w1, w
            y2, y1 = y1, out
            y[i] = out
        state[:] = [x1, x2, w1, w2, y1, y2]
        return y

    def push(self, samples: np.ndarray) -> None:
        if samples.size == 0:
            return
        if not np.isfinite(samples).all():
            self.nonfinite_samples += int((~np.isfinite(samples)).sum())
            samples = np.nan_to_num(samples, nan=0.0, posinf=0.0, neginf=0.0)
        data = samples.astype(np.float64, copy=False)
        n, channels = data.shape
        if channels != self.channels:
            raise RecoveryError(
                Code.LAYOUT_MISMATCH,
                f"loudness meter expects {self.channels} channels, got {channels}",
            )
        energy = np.zeros(n, dtype=np.float64)
        for c in range(channels):
            energy += self._filter_stage(data[:, c], self._state[c]) ** 2
        self.peak = max(self.peak, float(np.max(np.abs(data))) if data.size else 0.0)
        self.sample_count += n
        self._energy = np.concatenate([self._energy, energy])
        while self._energy.shape[0] >= self._block:
            self.powers.append(float(self._energy[: self._block].mean()))
            self._energy = self._energy[self._hop:]

    def result(self) -> tuple[float | None, float | None]:
        powers = list(self.powers)
        if not powers and self._energy.size:
            powers = [float(self._energy.mean())]
        lufs = _gated_loudness(powers)
        peak_dbfs = 20.0 * math.log10(self.peak) if self.peak > 0.0 else None
        return lufs, peak_dbfs


def compute_gain_db(
    input_lufs: float | None,
    input_peak_dbfs: float | None,
    *,
    target_lufs: float,
    peak_ceiling_dbfs: float,
) -> tuple[float, bool]:
    """Programme gain in dB, backed off to honour the peak ceiling."""
    if input_lufs is None:
        return 0.0, False
    gain = target_lufs - input_lufs
    peak = input_peak_dbfs if input_peak_dbfs is not None else -120.0
    limited = False
    if peak + gain > peak_ceiling_dbfs:
        gain = peak_ceiling_dbfs - peak
        limited = True
    return float(min(max(gain, MIN_GAIN_DB), MAX_GAIN_DB)), limited


@dataclass
class ChunkGainResult:
    samples: np.ndarray
    expected_samples: int
    written_samples: int
    boundary_error_samples: int
    clipped_samples: int = 0
    nonfinite_samples: int = 0

    def to_json(self) -> dict[str, object]:
        return {
            "expected_samples": self.expected_samples,
            "written_samples": self.written_samples,
            "boundary_error_samples": self.boundary_error_samples,
            "clipped_samples": self.clipped_samples,
            "nonfinite_samples": self.nonfinite_samples,
        }


@dataclass
class ChunkGainProcessor:
    """Applies a fixed programme gain per chunk with peak limiting.

    ``expected_output_samples`` is the exact length the caller plans for (from
    the rational segment duration at the target rate); a truncated final chunk
    writes fewer samples and the difference is reported, never hidden.
    """

    gain_db: float
    peak_ceiling_dbfs: float
    expected_output_samples: int
    clipped_samples: int = 0
    nonfinite_samples: int = 0
    results: list[ChunkGainResult] = field(default_factory=list)

    @property
    def linear_gain(self) -> float:
        return float(10.0 ** (self.gain_db / 20.0))

    def process(self, samples: np.ndarray) -> ChunkGainResult:
        if not np.isfinite(samples).all():
            self.nonfinite_samples += int((~np.isfinite(samples)).sum())
            samples = np.nan_to_num(samples, nan=0.0, posinf=0.0, neginf=0.0)
        out = samples.astype(np.float32, copy=True) * np.float32(self.linear_gain)
        ceiling = np.float32(10.0 ** (self.peak_ceiling_dbfs / 20.0))
        above = np.abs(out) > ceiling
        count = int(above.sum())
        if count:
            self.clipped_samples += count
            out = np.clip(out, -ceiling, ceiling)
        written = int(out.shape[0])
        result = ChunkGainResult(
            samples=out,
            expected_samples=self.expected_output_samples,
            written_samples=written,
            boundary_error_samples=written - self.expected_output_samples,
            clipped_samples=count,
        )
        self.results.append(result)
        return result

    def total_boundary_error(self) -> int:
        return sum(r.boundary_error_samples for r in self.results)


def expected_samples(duration_seconds: object, rate: int) -> int:
    """Exact expected sample count for a rational duration at ``rate``."""
    return int(round(rational(duration_seconds) * int(rate)))


__all__ = [
    "ChunkGainProcessor",
    "ChunkGainResult",
    "LoudnessMeter",
    "compute_gain_db",
    "expected_samples",
]
