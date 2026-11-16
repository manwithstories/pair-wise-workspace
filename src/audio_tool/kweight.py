"""ITU-R BS.1770-4 loudness: K-weighting, gating, RMS, peak and clipping.

The K-weighting biquads are derived analytically from the recommendation's
analog parameters (the same derivation libebur128 uses), so they are correct
at any sample rate instead of needing a 48 kHz coefficient table:

  stage 1  high shelf, ~1682 Hz, +4 dB
  stage 2  high pass,  ~38 Hz, Q 0.5

Gating follows BS.1770-4: 400 ms blocks on a 100 ms step, an absolute gate at
-70 LUFS, then a relative gate 10 LU below the ungated mean.
"""

from __future__ import annotations

import math
import operator
from array import array
from collections import deque
from typing import Sequence

from .model import ABSOLUTE_GATE, RELATIVE_GATE_OFFSET, LoudnessStats, linear_to_db

LUFS_OFFSET = -0.691
GATE_BLOCK_SECONDS = 0.4
GATE_STEP_SECONDS = 0.1
SHORT_TERM_SECONDS = 3.0

#: Blocks per gate window (400 ms / 100 ms).
BLOCKS_PER_GATE = int(round(GATE_BLOCK_SECONDS / GATE_STEP_SECONDS))
SHORT_TERM_STEPS = int(round(SHORT_TERM_SECONDS / GATE_STEP_SECONDS))

_SHELF_F0 = 1681.974450955533
_SHELF_GAIN_DB = 3.999843853973347
_SHELF_Q = 0.7071752369554196
_HIGHPASS_F0 = 38.13547087602444
_HIGHPASS_Q = 0.5003270373238773


class Biquad:
    """Direct-form-I biquad with persistent per-channel state."""

    __slots__ = ("b0", "b1", "b2", "a1", "a2", "x1", "x2", "y1", "y2")

    def __init__(self, b: Sequence[float], a: Sequence[float]):
        self.b0, self.b1, self.b2 = b
        self.a1, self.a2 = a[0], a[1]
        self.x1 = self.x2 = self.y1 = self.y2 = 0.0

    def process(self, samples: Sequence[float]) -> array:
        b0, b1, b2, a1, a2 = self.b0, self.b1, self.b2, self.a1, self.a2
        x1, x2, y1, y2 = self.x1, self.x2, self.y1, self.y2
        out = array("d", bytes(8 * len(samples)))
        index = 0
        for x0 in samples:
            y0 = b0 * x0 + b1 * x1 + b2 * x2 - a1 * y1 - a2 * y2
            out[index] = y0
            index += 1
            x2, x1 = x1, x0
            y2, y1 = y1, y0
        self.x1, self.x2, self.y1, self.y2 = x1, x2, y1, y2
        return out


def _shelf_coefficients(sample_rate: int) -> tuple[tuple[float, ...], tuple[float, ...]]:
    k = math.tan(math.pi * _SHELF_F0 / sample_rate)
    vh = 10.0 ** (_SHELF_GAIN_DB / 20.0)
    vb = vh ** 0.4996667741545416
    a0 = 1.0 + k / _SHELF_Q + k * k
    b = ((vh + vb * k / _SHELF_Q + k * k) / a0,
         2.0 * (k * k - vh) / a0,
         (vh - vb * k / _SHELF_Q + k * k) / a0)
    a = (2.0 * (k * k - 1.0) / a0, (1.0 - k / _SHELF_Q + k * k) / a0)
    return b, a


def _highpass_coefficients(sample_rate: int) -> tuple[tuple[float, ...], tuple[float, ...]]:
    k = math.tan(math.pi * _HIGHPASS_F0 / sample_rate)
    a0 = 1.0 + k / _HIGHPASS_Q + k * k
    b = (1.0, -2.0, 1.0)
    a = (2.0 * (k * k - 1.0) / a0, (1.0 - k / _HIGHPASS_Q + k * k) / a0)
    return b, a


class KWeighting:
    """Per-channel cascade of the two BS.1770 filters."""

    def __init__(self, channels: int, sample_rate: int):
        shelf_b, shelf_a = _shelf_coefficients(sample_rate)
        hp_b, hp_a = _highpass_coefficients(sample_rate)
        self._shelf = [Biquad(shelf_b, shelf_a) for _ in range(channels)]
        self._highpass = [Biquad(hp_b, hp_a) for _ in range(channels)]

    def process(self, planes: Sequence[Sequence[float]], frames: int) -> list[array]:
        out = []
        for chan, plane in enumerate(planes):
            shelf = self._shelf[chan].process(plane[:frames])
            out.append(self._highpass[chan].process(shelf))
        return out

    def k_weighted_gain(self, frequency: float, sample_rate: int) -> float:
        """Magnitude response of the cascade at ``frequency`` (for tests)."""

        z = complex(math.cos(-2 * math.pi * frequency / sample_rate),
                    math.sin(-2 * math.pi * frequency / sample_rate))

        def response(coeffs_b, coeffs_a, state):
            num = coeffs_b[0] + coeffs_b[1] * z + coeffs_b[2] * z * z
            den = 1.0 + coeffs_a[0] * z + coeffs_a[1] * z * z
            return num / den

        total = response(*_shelf_coefficients(sample_rate), None)
        total *= response(*_highpass_coefficients(sample_rate), None)
        return abs(total)


__all__ = [
    "Biquad",
    "KWeighting",
    "_highpass_coefficients",
    "_shelf_coefficients",
]
