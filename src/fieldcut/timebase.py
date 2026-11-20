"""Shared time base: tick conversions, PTS/DTS unwrap and clock drift fitting.

Every stage of the pipeline imports its time model from here so that probe,
alignment, segmentation, the encoder ladder and manifest validation all speak
the same unit (integer 90 kHz ticks, or exact ``Fraction`` values).

Two timestamp defects are deliberately distinguished:

* **wrap** – a capture counter reaching its modulus and restarting near zero.
  The jump magnitude is close to a full modulus, and unwrapping restores a
  monotonic axis.
* **genuine regression** – a small backward step (reorder / duplicate / live
  clock adjustment). It is *not* a wrap; the value is left in place and the
  caller decides how to quarantine it.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from fractions import Fraction

from . import TICKS_PER_MS, TICKS_PER_SECOND

# 33-bit capture counters are the worst case seen on field recorders.
DEFAULT_WRAP_MODULUS = 1 << 33
HALF_MODULUS = DEFAULT_WRAP_MODULUS // 2


# --------------------------------------------------------------------------
# unit conversions (everything stays integer until reporting)
# --------------------------------------------------------------------------

def ms_to_ticks(ms: float) -> int:
    return int(round(ms * TICKS_PER_MS))


def seconds_to_ticks(seconds: float) -> int:
    return int(round(seconds * TICKS_PER_SECOND))


def ticks_to_seconds(ticks: int | Fraction) -> Fraction:
    return Fraction(ticks) / TICKS_PER_SECOND


def ticks_to_ms(ticks: int | Fraction) -> Fraction:
    return Fraction(ticks) / TICKS_PER_MS


def ticks_to_ms_float(ticks: int | Fraction) -> float:
    return float(ticks_to_ms(ticks))


def time_base_to_ticks(value: int, time_base: Fraction) -> int:
    """Map ``value`` expressed in ``time_base`` units onto 90 kHz ticks."""
    return int(round(Fraction(value) * TICKS_PER_SECOND * time_base))


# --------------------------------------------------------------------------
# timestamp unwrap
# --------------------------------------------------------------------------

@dataclass
class UnwrapResult:
    """Monotonic-ish unwrapped axis plus the locations of each defect."""

    values: list[int]
    wrap_indices: list[int] = field(default_factory=list)
    regression_indices: list[int] = field(default_factory=list)


def unwrap_timestamps(
    seq: list[int],
    modulus: int = DEFAULT_WRAP_MODULUS,
) -> UnwrapResult:
    """Unwrap a modular counter into a continuous tick axis.

    A forward jump larger than half the modulus is a *backward wrap*; a
    backward jump larger than half the modulus is a *forward wrap* (a late
    packet carrying an old counter). Smaller backward steps are genuine
    regressions: recorded, but never folded into the wrap offset.
    """
    res = UnwrapResult(values=[])
    if not seq:
        return res
    half = modulus // 2
    offset = 0
    prev = seq[0]
    res.values.append(prev)
    for i in range(1, len(seq)):
        cur = seq[i]
        delta = cur - prev
        if delta < -half:
            offset += modulus            # counter wrapped forward past zero
            res.wrap_indices.append(i)
        elif delta > half:
            offset -= modulus            # late pre-wrap packet arrived late
            res.wrap_indices.append(i)
        elif delta < 0:
            res.regression_indices.append(i)
        res.values.append(cur + offset)
        prev = cur
    return res


# --------------------------------------------------------------------------
# drift estimation
# --------------------------------------------------------------------------

@dataclass
class DriftFit:
    """Linear fit ``reference = slope * capture + intercept`` with confidence.

    ``slope`` is reference ticks per capture tick (~1.0); ``rate_ppm`` is the
    fractional drift in parts per million. The half-width confidence interval
    is derived from the residual standard error of the least-squares fit.
    """

    slope: Fraction
    intercept: Fraction
    rate_ppm: float
    ci_half_width_ppm: float
    n: int
    residual_rms_ms: float

    def map(self, capture_ticks: int) -> Fraction:
        return self.slope * capture_ticks + self.intercept


def fit_drift(capture_ticks: list[int], reference_ticks: list[int]) -> DriftFit:
    """Fit one linear clock mapping and its 95% drift confidence interval.

    Rational arithmetic is used for the line itself; the confidence interval
    uses the standard OLS residual variance (Student-t approximated by 1.96
    for the well-populated observation windows produced by real recordings).
    """
    n = len(capture_ticks)
    if n != len(reference_ticks):
        raise ValueError("capture/reference length mismatch")
    if n == 0:
        raise ValueError("cannot fit drift over an empty observation window")
    if n == 1:
        return DriftFit(Fraction(1),
                        Fraction(reference_ticks[0] - capture_ticks[0]),
                        0.0, math.inf, 1, 0.0)

    sx = sum(capture_ticks)
    sy = sum(reference_ticks)
    sxx = sum(x * x for x in capture_ticks)
    sxy = sum(x * y for x, y in zip(capture_ticks, reference_ticks))
    denom = n * sxx - sx * sx
    if denom == 0:
        slope = Fraction(1)
        intercept = Fraction(sy - sx) // n
    else:
        slope = Fraction(n * sxy - sx * sy, denom)
        intercept = Fraction(sy - slope * sx, n)

    preds = [slope * x + intercept for x in capture_ticks]
    ss_res = sum(float(y - p) ** 2
                 for y, p in zip(reference_ticks, preds))
    rms = math.sqrt(ss_res / n) / TICKS_PER_MS
    # Var(slope) = sigma^2 / Sxx_demean; 1.96 -> 95% interval.
    sxx_demean = sxx - sx * sx / n
    if sxx_demean > 0 and n > 2:
        sigma2 = ss_res / (n - 2)
        slope_se = math.sqrt(sigma2 / sxx_demean)
        ci_half = 1.96 * slope_se * 1_000_000.0
    else:
        ci_half = math.inf
    return DriftFit(slope, intercept,
                    rate_ppm=(float(slope) - 1.0) * 1_000_000.0,
                    ci_half_width_ppm=ci_half, n=n, residual_rms_ms=rms)


@dataclass
class FrameRateProfile:
    """Measured frame cadence. ``variable`` flags non-constant frame deltas."""

    nominal_fps: float
    median_delta_ticks: int
    variable: bool
    deltas_ticks: list[int] = field(default_factory=list)


def profile_frame_rate(pts_ticks: list[int]) -> FrameRateProfile:
    """Describe cadence from real frame PTS — never assume a fixed frame rate."""
    if len(pts_ticks) < 2:
        return FrameRateProfile(0.0, 0, False, [])
    deltas = [b - a for a, b in zip(pts_ticks, pts_ticks[1:])]
    positive = [d for d in deltas if d > 0] or deltas
    ordered = sorted(positive)
    median = ordered[len(ordered) // 2]
    nominal = TICKS_PER_SECOND / median if median else 0.0
    # VFR when any positive delta deviates from the median by even one tick:
    # a constant frame rate produces an exactly constant delta.
    variable = any(d > 0 and d != median for d in deltas)
    return FrameRateProfile(nominal, median, variable, deltas)
