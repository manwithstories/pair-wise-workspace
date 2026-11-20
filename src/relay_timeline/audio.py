"""Audio track alignment, A/V drift estimation and PCM statistics.

Decoded :class:`~relay_timeline.decode.AudioItem` chunks are placed on a
gap-free sample grid (zero-filled holes), written as PCM WAV through
soundfile and summarized with peak/RMS statistics.  Each chunk's
presentation time is paired with the video reference clock so the
:class:`~relay_timeline.clock.DriftEstimator` can quantify linear node
clock drift in ppm.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

import numpy as np
import soundfile as sf

from .clock import DriftEstimator, DriftResult
from .streamio import AudioItem


@dataclass(slots=True)
class AudioStats:
    sample_rate: int
    samples: int
    peak: float
    rms: float
    drift_ppm: float
    drift_slope: float
    drift_samples: int
    isolated: bool = False

    def to_dict(self) -> dict:
        return {
            "sample_rate": self.sample_rate,
            "samples": self.samples,
            "peak": self.peak,
            "rms": self.rms,
            "drift_ppm": self.drift_ppm,
            "drift_slope": self.drift_slope,
            "drift_samples": self.drift_samples,
            "isolated": self.isolated,
        }


@dataclass(slots=True)
class AudioAssembler:
    """Reconstruct one continuous mono PCM track from decoded chunks."""

    sample_rate: int
    ppm_limit: float
    _chunks: list[AudioItem] = field(default_factory=list)
    _groups: dict[int, list[tuple[float, float]]] = field(default_factory=dict)
    _total_samples: int = 0

    def add(self, item: AudioItem, reference_s: Fraction) -> None:
        self._chunks.append(item)
        # Within-segment (nominal, observed) pairs.  Fitting inside each
        # segment makes the per-segment intercept absorb container offsets
        # so only the genuine linear clock rate difference drives the slope.
        self._groups.setdefault(item.sequence, []).append(
            (float(item.nominal_seconds), float(item.pts_seconds)))

    def _fit_within_segments(self) -> DriftResult | None:
        xs: list[float] = []
        ys: list[float] = []
        for pairs in self._groups.values():
            if len(pairs) < 2:
                continue
            x0, y0 = pairs[0]
            for x, y in pairs:
                xs.append(x - x0)
                ys.append(y - y0)
        if len(xs) < 2:
            return None
        estimator = DriftEstimator(self.ppm_limit)
        for x, y in zip(xs, ys):
            estimator.add(Fraction(x).limit_denominator(10 ** 9),
                          Fraction(y).limit_denominator(10 ** 9))
        return estimator.fit()

    def materialize(self) -> tuple[np.ndarray, DriftResult | None]:
        """Lay chunks on a gap-free global sample grid; holes go silent.

        A segment's audio restarts on its local grid, so the global sample
        position is ``segment_base + in_segment_offset``.  Nominal seconds
        already encode both, so they map directly onto the global grid.
        """
        fit = self._fit_within_segments()
        if not self._chunks:
            return np.zeros(0, dtype=np.int16), fit
        placements: list[tuple[int, np.ndarray]] = []
        grid_len = 0
        for item in self._chunks:
            start = int(item.nominal_seconds * item.sample_rate)
            placements.append((start, item.pcm))
            grid_len = max(grid_len, start + len(item.pcm))
        grid = np.zeros(grid_len, dtype=np.int16)
        for start, pcm in placements:
            end = min(grid_len, start + len(pcm))
            grid[start:end] = pcm[: end - start]
        self._total_samples = grid_len
        return grid, fit

    def write_wav(self, path) -> AudioStats:
        pcm, fit = self.materialize()
        peak = float(np.max(np.abs(pcm)).astype(np.float64) / 32768.0) \
            if pcm.size else 0.0
        rms = float(np.sqrt(np.mean((pcm.astype(np.float64) / 32768.0) ** 2))) \
            if pcm.size else 0.0
        sf.write(str(path), pcm, self.sample_rate, subtype="PCM_16")
        if fit is None:
            return AudioStats(self.sample_rate, pcm.size, peak, rms,
                              0.0, 1.0, 0)
        isolated = fit.exceeded
        return AudioStats(
            sample_rate=self.sample_rate,
            samples=pcm.size,
            peak=peak,
            rms=rms,
            drift_ppm=fit.ppm,
            drift_slope=fit.slope,
            drift_samples=fit.samples,
            isolated=isolated,
        )
