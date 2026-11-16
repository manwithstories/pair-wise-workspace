"""True-peak metering by 4x oversampled windowed-sinc interpolation.

Shared by the measuring pass and the gain guard: the meter reports the
inter-sample peak that a downstream encoder can actually reconstruct, which is
what the normalization ceiling must respect.
"""

from __future__ import annotations

import math
import operator
from typing import Sequence

from .dspmath import kaiser_window, sinc


class TruePeakMeter:
    """Inter-sample peak by 4x oversampling with a windowed-sinc interpolator.

    One coefficient bank per fractional phase, each normalised to unit DC gain
    so a tone interpolates without a level bias.  Interpolation is *deferred*
    by ``HALF_TAPS`` samples: a position is only evaluated once the whole tap
    window holds real samples, so a block boundary can never corrupt the
    reading (padding the block instead would invent an overshoot at every
    edge).  The original sample values are also candidates, so the meter never
    under-reads the sample peak.
    """

    OVERSAMPLE = 4
    HALF_TAPS = 8

    def __init__(self, channels: int):
        self._peaks = [0.0] * channels
        self._branches = _interpolator_banks(self.OVERSAMPLE, self.HALF_TAPS)
        half = self.HALF_TAPS
        # history[i] holds the sample whose absolute index is ``_base + i``.
        # Leading zeros let positions [0, ...) evaluate with a full window.
        self._history: list[list[float]] = [[0.0] * half for _ in range(channels)]
        self._base = -half
        self._cursor = 0          # next inter-sample position to evaluate
        self._total = 0           # real samples appended so far

    def process(self, planes: Sequence[Sequence[float]], frames: int) -> None:
        # Original samples are always candidates: interpolation between two
        # samples can legitimately land *below* an exact sample peak, so
        # without this the meter could under-read a full-scale sample.
        for chan, plane in enumerate(planes):
            peak = self._peaks[chan]
            for value in plane[:frames]:
                magnitude = value if value >= 0.0 else -value
                if magnitude > peak:
                    peak = magnitude
            self._peaks[chan] = peak
            self._history[chan].extend(plane[:frames])
        self._total += frames
        self._evaluate(final=False)

    def flush(self) -> None:
        """Zero-pad the tail and evaluate the final inter-sample positions."""

        half = self.HALF_TAPS
        for history in self._history:
            history.extend([0.0] * half)
        self._evaluate(final=True)

    @property
    def peaks(self) -> list[float]:
        return self._peaks

    def _evaluate(self, final: bool) -> None:
        half = self.HALF_TAPS
        branches = self._branches
        if final:
            # Positions strictly inside the signal: the last meaningful
            # inter-sample point sits between samples total-2 and total-1.
            limit = max(0, self._total - 1)
        else:
            # A window for position n spans samples n-half+1 .. n+half, so it
            # only holds real data once n+half <= total-1.
            limit = max(0, self._total - half)
        if limit <= self._cursor:
            return
        for chan, history in enumerate(self._history):
            peak = self._peaks[chan]
            for position in range(self._cursor, limit):
                start = position - half + 1 - self._base
                window = history[start:start + 2 * half]
                for branch in branches:
                    acc = sum(map(operator.mul, branch, window))
                    magnitude = acc if acc >= 0.0 else -acc
                    if magnitude > peak:
                        peak = magnitude
            self._peaks[chan] = peak
        self._cursor = limit
        if not final:
            self._trim()

    def _trim(self) -> None:
        """Discard history no future window can reach."""

        half = self.HALF_TAPS
        keep_from = self._cursor - half + 1
        drop = keep_from - self._base
        if drop <= 0:
            return
        for history in self._history:
            del history[:drop]
        self._base = keep_from

    @property
    def peaks(self) -> list[float]:
        return self._peaks


def _interpolator_banks(oversample: int, half: int) -> list[tuple[float, ...]]:
    """Unit-gain polyphase sinc banks for the fractional offsets 1..OS-1."""

    span = oversample * half
    banks = []
    for phase in range(1, oversample):
        coeffs = []
        for tap in range(-half + 1, half + 1):
            t = oversample * tap - phase
            coeffs.append(sinc(t / oversample) * kaiser_window(t / span, 7.0))
        total = math.fsum(coeffs)
        banks.append(tuple(value / total for value in coeffs))
    return banks


