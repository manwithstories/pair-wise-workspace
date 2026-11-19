"""Integer-tick time base, 33-bit timestamp unwrap and clock drift fitting.

All timeline math uses integer ticks (or exact Fractions) so that floating
point accumulation can never shift a frame boundary.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

TICKS_PER_SECOND = 90_000  # unified timeline tick rate (MPEG-compatible)
TICKS_PER_MS = TICKS_PER_SECOND // 1000
WRAP_MODULUS = 1 << 33  # 33-bit capture timestamp wraps here
WRAP_HALF = WRAP_MODULUS // 2


def ms_to_ticks(ms: int | float) -> int:
    return int(round(ms * TICKS_PER_MS))


def ticks_to_ms(ticks: int) -> Fraction:
    return Fraction(ticks, TICKS_PER_MS)


def unwrap_timestamps(seq: list[int]) -> tuple[list[int], list[int]]:
    """Unwrap 33-bit timestamps.

    Returns (unwrapped, wrap_indices). A forward jump larger than half the
    modulus is treated as a *backward wrap* correction; a backward jump whose
    magnitude exceeds half the modulus is a *forward wrap*. Small backward
    jumps are genuine regressions (reorder candidates) and are left as-is.
    """
    if not seq:
        return [], []
    out = [0] * len(seq)
    wraps: list[int] = []
    offset = 0
    prev = seq[0]
    out[0] = prev
    for i in range(1, len(seq)):
        cur = seq[i]
        delta = cur - prev
        if delta < -WRAP_HALF:
            offset += WRAP_MODULUS  # forward wrap
            wraps.append(i)
        elif delta > WRAP_HALF:
            offset -= WRAP_MODULUS  # backward wrap (rare, late old packet)
            wraps.append(i)
        out[i] = cur + offset
        prev = cur
    return out, wraps


@dataclass
class DriftSegment:
    """One linear capture->reference clock segment: ref_ticks = slope*x + icept."""

    slope: Fraction  # reference ticks per capture tick (~1.0)
    icept: Fraction  # intercept in reference ticks
    start_index: int
    end_index: int  # exclusive

    def map(self, capture_ticks: int) -> Fraction:
        return self.slope * capture_ticks + self.icept


@dataclass
class DriftModel:
    """Piecewise-linear drift fit; jumps in the capture clock split segments."""

    segments: list[DriftSegment] = field(default_factory=list)

    def map(self, capture_ticks: int, index_hint: int | None = None) -> Fraction:
        seg = self.segment_for(capture_ticks, index_hint)
        return seg.map(capture_ticks)

    def segment_for(self, capture_ticks: int, index_hint: int | None = None) -> DriftSegment:
        if not self.segments:
            raise ValueError("empty drift model")
        if index_hint is not None:
            for seg in self.segments:
                if seg.start_index <= index_hint < seg.end_index:
                    return seg
        return self.segments[-1]


def _fit_line(xs: list[int], ys: list[int]) -> tuple[Fraction, Fraction]:
    """Exact least-squares fit y = a*x + b using rational arithmetic."""
    n = len(xs)
    if n == 1:
        return Fraction(1), Fraction(ys[0] - xs[0])
    sx = sum(xs)
    sy = sum(ys)
    sxx = sum(x * x for x in xs)
    sxy = sum(x * y for x, y in zip(xs, ys))
    denom = n * sxx - sx * sx
    if denom == 0:
        return Fraction(1), Fraction(ys[0] - xs[0])
    a = Fraction(n * sxy - sx * sy, denom)
    b = Fraction(sy - a * sx, n)
    return a, b


def fit_drift(
    capture_ticks: list[int],
    ref_ticks: list[int],
    jump_threshold_ticks: int = 50 * TICKS_PER_MS,
) -> tuple[DriftModel, list[int]]:
    """Fit piecewise-linear drift; returns (model, segment_break_indices).

    A new segment starts when the residual against the running fit exceeds
    ``jump_threshold_ticks`` (a capture-clock step, not smooth drift).
    """
    if len(capture_ticks) != len(ref_ticks):
        raise ValueError("capture/ref length mismatch")
    model = DriftModel()
    breaks: list[int] = []
    n = len(capture_ticks)
    if n == 0:
        return model, breaks
    seg_start = 0
    i = 1
    while i < n:
        a, b = _fit_line(capture_ticks[seg_start:i], ref_ticks[seg_start:i])
        residual = abs(a * capture_ticks[i] + b - ref_ticks[i])
        if residual > jump_threshold_ticks and i - seg_start >= 2:
            a, b = _fit_line(capture_ticks[seg_start:i], ref_ticks[seg_start:i])
            model.segments.append(DriftSegment(a, b, seg_start, i))
            breaks.append(i)
            seg_start = i
        i += 1
    a, b = _fit_line(capture_ticks[seg_start:n], ref_ticks[seg_start:n])
    model.segments.append(DriftSegment(a, b, seg_start, n))
    return model, breaks
