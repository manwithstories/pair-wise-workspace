"""Node clock drift estimation and modular timestamp unwrapping.

Two cooperating primitives live here:

* :class:`TimestampUnwrapper` turns a 32-bit (or any modulus) wrapping counter
  into a monotonically increasing 64-bit tick count, flagging duplicates and
  backwards jumps without hiding them.
* :class:`ClockFitter` maps unwrapped node ticks onto a reference wall clock
  using anchor pairs.  It estimates drift in ppm, quantifies confidence and an
  error interval, and reports a drift *break* when later anchors stop lying on
  the fitted line (clock adjustment / oscillator jump).
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field

from .timeline import wrap_delta

DEFAULT_MODULUS = 1 << 32
#: Pessimistic free-running drift assumed when only one anchor is available.
NOMINAL_DRIFT_PPM = 1000.0
#: Residual (seconds) above which a drift break is declared even on tiny data.
BREAK_TOLERANCE_S = 0.020


@dataclass(slots=True)
class Anchor:
    """One known correspondence: node tick ``node_ticks`` at ref time."""

    node_ticks: int
    ref_seconds: float
    node_rate: int = 90000


@dataclass(slots=True)
class UnwrapResult:
    unwrapped: int
    delta: int
    wrapped: bool
    duplicate: bool
    backwards: bool


class TimestampUnwrapper:
    """Stateful shortest-path unwrapper for a modular counter."""

    def __init__(self, modulus: int = DEFAULT_MODULUS) -> None:
        if modulus <= 1:
            raise ValueError("modulus must be > 1")
        self.modulus = modulus
        self._last_raw: int | None = None
        self._total = 0
        self.wrap_count = 0

    def step(self, raw: int) -> UnwrapResult:
        raw %= self.modulus
        if self._last_raw is None:
            self._total = raw
            self._last_raw = raw
            return UnwrapResult(raw, 0, False, False, False)
        delta = wrap_delta(raw, self._last_raw, self.modulus)
        # A forward wrap looks like a small positive shortest-path delta while
        # the raw counter drops; a negative delta is a genuine backwards jump.
        wrapped = delta > 0 and raw < self._last_raw
        duplicate = delta == 0
        backwards = delta < 0
        if wrapped:
            self.wrap_count += 1
        self._total += delta
        self._last_raw = raw
        return UnwrapResult(self._total, delta, wrapped, duplicate, backwards)

    @property
    def value(self) -> int:
        return self._total


@dataclass(slots=True)
class ClockFit:
    slope: float
    intercept: float
    drift_ppm: float
    confidence: float
    error_low_s: float
    error_high_s: float
    break_ref_s: float | None
    anchor_count: int
    note: str = ""
    residuals: list[float] = field(default_factory=list)

    def to_ref(self, node_seconds: float) -> float:
        return self.slope * node_seconds + self.intercept

    def bounds(self, node_seconds: float) -> tuple[float, float]:
        center = self.to_ref(node_seconds)
        return center + self.error_low_s, center + self.error_high_s

    def to_dict(self) -> dict:
        return {
            "drift_ppm": round(self.drift_ppm, 3),
            "confidence": round(self.confidence, 4),
            "error_interval_ms": [
                round(self.error_low_s * 1000, 3),
                round(self.error_high_s * 1000, 3),
            ],
            "drift_break_ref_s": self.break_ref_s,
            "anchor_count": self.anchor_count,
            "note": self.note,
        }


class ClockFitter:
    """Fit ``ref_seconds = slope * node_seconds + intercept`` from anchors."""

    def __init__(self, anchors: list[Anchor] | None = None) -> None:
        self.anchors: list[Anchor] = list(anchors or [])

    def add(self, anchor: Anchor) -> None:
        self.anchors.append(anchor)

    def fit(self) -> ClockFit:
        n = len(self.anchors)
        if n == 0:
            return ClockFit(1.0, 0.0, 0.0, 0.0, -math.inf, math.inf, None, 0,
                            "no anchors: reference time unconstrained")
        xs = [a.node_ticks / a.node_rate for a in self.anchors]
        ys = [a.ref_seconds for a in self.anchors]
        if n == 1:
            intercept = ys[0] - xs[0]
            span = max(abs(xs[0]), 1.0)
            err = NOMINAL_DRIFT_PPM * 1e-6 * span
            return ClockFit(1.0, intercept, 0.0, 0.3, -err, err, None, 1,
                            "single anchor: nominal rate assumed, drift unmeasured")
        slope, intercept = _least_squares(xs, ys)
        residuals = [y - (slope * x + intercept) for x, y in zip(xs, ys)]
        spread = math.sqrt(sum(r * r for r in residuals) / n)
        peak = max(abs(r) for r in residuals)
        break_idx = self._detect_break(xs, ys)
        break_ref = ys[break_idx] if break_idx is not None else None
        # Confidence degrades with residual spread and anchor scarcity.
        confidence = max(0.05, min(1.0, 1.0 - spread / 0.050)) * min(1.0, n / 6.0)
        if break_idx is not None:
            confidence = min(confidence, 0.4)
        pad = max(peak, BREAK_TOLERANCE_S if break_idx is not None else spread * 2, 1e-4)
        note = "drift break detected" if break_idx is not None else "anchors consistent"
        return ClockFit(slope, intercept, (slope - 1.0) * 1e6, confidence,
                        -pad, pad, break_ref, n, note, residuals)

    @staticmethod
    def _detect_break(xs: list[float], ys: list[float]) -> int | None:
        """Fit the head anchors, then find a persistent departure from that line."""
        n = len(xs)
        if n < 4:
            return None
        head_n = max(2, n // 3)
        hs, hi = _least_squares(xs[:head_n], ys[:head_n])
        head_res = [y - (hs * x + hi) for x, y in zip(xs[:head_n], ys[:head_n])]
        band = math.sqrt(sum(r * r for r in head_res) / head_n) * 3 + BREAK_TOLERANCE_S
        for i in range(head_n, n):
            outside = abs(ys[i] - (hs * xs[i] + hi)) > band
            if outside and all(
                abs(ys[j] - (hs * xs[j] + hi)) > band for j in range(i, n)
            ):
                return i
        return None


def _least_squares(xs: list[float], ys: list[float]) -> tuple[float, float]:
    n = len(xs)
    mx = sum(xs) / n
    my = sum(ys) / n
    sxx = sum((x - mx) ** 2 for x in xs)
    if sxx == 0:
        return 1.0, my - mx
    sxy = sum((x - mx) * (y - my) for x, y in zip(xs, ys))
    slope = sxy / sxx
    return slope, my - slope * mx
