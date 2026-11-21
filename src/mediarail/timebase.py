"""Unified rational time base, clock drift estimation and wrap unwrapping.

All timeline math is done with :class:`fractions.Fraction` so that rescaling
between container time bases never accumulates floating point error.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Iterable, Sequence

# Drift above this magnitude (parts per million) triggers segmented fitting.
DRIFT_SEGMENT_PPM = 200.0

# Window (in samples) used for segmented drift fitting.
_SEGMENT_WINDOW = 64


def to_seconds(value: int | Fraction | None, time_base: Fraction) -> Fraction | None:
    """Convert a timestamp in ``time_base`` units to exact seconds."""
    if value is None:
        return None
    return Fraction(value) * Fraction(time_base)


def rescale(value: int | Fraction, src: Fraction, dst: Fraction) -> int:
    """Rescale ``value`` from time base ``src`` to ``dst``, rounding to nearest."""
    exact = Fraction(value) * Fraction(src) / Fraction(dst)
    return _round_half_up(exact)


def _round_half_up(x: Fraction) -> int:
    floor = x.numerator // x.denominator
    return floor + 1 if x - floor >= Fraction(1, 2) else floor


@dataclass
class DriftSegment:
    """One fitted clock segment: corrected = raw * slope + offset (seconds)."""

    start_index: int
    end_index: int
    slope: float
    offset: float

    @property
    def drift_ppm(self) -> float:
        return (self.slope - 1.0) * 1e6

    def apply(self, raw_seconds: float) -> float:
        return raw_seconds * self.slope + self.offset


@dataclass
class DriftEstimate:
    """Result of drift estimation for one track."""

    segments: list[DriftSegment] = field(default_factory=list)

    @property
    def segmented(self) -> bool:
        return len(self.segments) > 1

    @property
    def max_drift_ppm(self) -> float:
        if not self.segments:
            return 0.0
        return max(abs(s.drift_ppm) for s in self.segments)

    def correct(self, raw_seconds: float, sample_index: int) -> float:
        """Apply the correction valid at ``sample_index`` to a raw timestamp."""
        if not self.segments:
            return raw_seconds
        seg = self.segments[0]
        for s in self.segments:
            if s.start_index <= sample_index:
                seg = s
            else:
                break
        return seg.apply(raw_seconds)


def _fit_line(xs: Sequence[float], ys: Sequence[float]) -> tuple[float, float]:
    """Least-squares fit of ys = slope * xs + intercept."""
    n = len(xs)
    if n == 0:
        return 1.0, 0.0
    if n == 1:
        return 1.0, ys[0] - xs[0]
    mx = sum(xs) / n
    my = sum(ys) / n
    sxx = sum((x - mx) ** 2 for x in xs)
    if sxx == 0.0:
        return 1.0, my - mx
    sxy = sum((x - mx) * (y - my) for x, y in zip(xs, ys))
    slope = sxy / sxx
    return slope, my - slope * mx


def estimate_drift(
    raw_seconds: Sequence[float], reference_seconds: Sequence[float]
) -> DriftEstimate:
    """Estimate clock offset/drift of ``raw_seconds`` against a reference.

    ``reference_seconds`` is where each sample *should* land (e.g. derived
    from sample counts).  A single global fit is used when the drift stays
    within ``DRIFT_SEGMENT_PPM``; otherwise the track is fitted piecewise and
    the segment boundaries are reported.
    """
    n = min(len(raw_seconds), len(reference_seconds))
    if n < 2:
        return DriftEstimate()
    xs = [float(raw_seconds[i]) for i in range(n)]
    ys = [float(reference_seconds[i]) for i in range(n)]
    slope, offset = _fit_line(xs, ys)
    if abs(slope - 1.0) * 1e6 <= DRIFT_SEGMENT_PPM or n < 2 * _SEGMENT_WINDOW:
        return DriftEstimate([DriftSegment(0, n - 1, slope, offset)])
    segments: list[DriftSegment] = []
    start = 0
    while start < n:
        end = min(start + _SEGMENT_WINDOW, n)
        s, o = _fit_line(xs[start:end], ys[start:end])
        segments.append(DriftSegment(start, end - 1, s, o))
        start = end
    return DriftEstimate(segments)


class WrapEvent:
    """Classification of a timestamp discontinuity."""

    WRAP = "wrap"            # genuine counter wrap at the modulus boundary
    OUT_OF_ORDER = "out_of_order"  # small backwards step, likely reordering
    RESET = "reset"          # counter restarted far from the modulus boundary
    AMBIGUOUS = "ambiguous"  # cannot decide; evidence kept, not auto-repaired


@dataclass
class UnwrapResult:
    """Unwrapped timestamps plus the discontinuity events observed."""

    values: list[int]
    events: list[tuple[int, str, int, int]]  # (index, kind, previous, current)


def unwrap_timestamps(
    timestamps: Iterable[int],
    modulus: int,
    reorder_tolerance: int | None = None,
) -> UnwrapResult:
    """Unwrap a wrapped counter and classify each discontinuity.

    A drop that lands within ``modulus`` of the previous value is a genuine
    wrap; a small drop (<= ``reorder_tolerance``, default 1% of the modulus)
    is packet reordering; a large drop far from the boundary is a reset.
    When a drop is both small in absolute terms and large relative to the
    observed step cadence the event is marked ambiguous and the original
    value is preserved.
    """
    if reorder_tolerance is None:
        reorder_tolerance = max(1, modulus // 100)
    raw = list(timestamps)
    if not raw:
        return UnwrapResult([], [])
    out = [raw[0]]
    events: list[tuple[int, str, int, int]] = []
    wraps = 0
    for i in range(1, len(raw)):
        prev_raw = raw[i - 1]
        cur = raw[i]
        if cur >= prev_raw:
            out.append(cur + wraps * modulus)
            continue
        drop = prev_raw - cur
        distance_to_boundary = modulus - prev_raw
        if drop >= distance_to_boundary and distance_to_boundary <= reorder_tolerance:
            # Counter ran into the modulus boundary: genuine wrap.
            wraps += 1
            events.append((i, WrapEvent.WRAP, prev_raw, cur))
            out.append(cur + wraps * modulus)
        elif drop <= reorder_tolerance:
            events.append((i, WrapEvent.OUT_OF_ORDER, prev_raw, cur))
            out.append(cur + wraps * modulus)
        elif drop < modulus // 4 and distance_to_boundary > modulus // 4:
            # Mid-range drop that fits neither wrap nor reset cleanly.
            events.append((i, WrapEvent.AMBIGUOUS, prev_raw, cur))
            out.append(cur + wraps * modulus)
        else:
            events.append((i, WrapEvent.RESET, prev_raw, cur))
            wraps = 0
            out.append(cur)
    return UnwrapResult(out, events)
