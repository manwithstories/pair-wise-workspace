"""Common rational clock, 33-bit wrap unwrapping and drift estimation.

Every media timestamp is normalized to a rational number of seconds
(:class:`fractions.Fraction`) so no floating point error accumulates over
a long timeline.  MPEG-2 PTS values are modulo 2**33 at 90 kHz; the
:class:`WrapUnroller` expands them into a monotonic 64-bit tick space.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction

from .config import MPEG_PTS_MOD
from .errors import ErrorCode, RecoverableError

# Canonical internal tick rate: MPEG 90 kHz.
HZ_90K = Fraction(90_000)


def rational(numerator: int, denominator: int = 1) -> Fraction:
    """Build an exact rational, rejecting degenerate time bases."""
    if denominator <= 0:
        raise ValueError(f"time base denominator must be positive, got {denominator}")
    return Fraction(numerator, denominator)


def ticks_to_seconds(ticks: int, time_base: Fraction) -> Fraction:
    """Convert ``ticks`` at ``time_base`` seconds/tick to exact seconds."""
    return Fraction(ticks) * time_base


class WrapUnroller:
    """Expand modulo-2**33 PTS values into a monotonic tick stream.

    The unroller only advances: a jump larger than half the modulus is
    interpreted as a wrap, a small non-advancing value is reported as
    duplicate/backward by the caller via :meth:`classify`.
    """

    def __init__(self, modulus: int = MPEG_PTS_MOD) -> None:
        if modulus <= 0:
            raise ValueError("modulus must be positive")
        self.modulus = modulus
        self.half = modulus // 2
        self.wraps = 0
        self._last_raw: int | None = None
        self._last_out: int | None = None

    @property
    def last_output(self) -> int | None:
        return self._last_out

    def reset(self) -> None:
        self.wraps = 0
        self._last_raw = None
        self._last_out = None

    def unwrap(self, raw_pts: int) -> int:
        """Return a monotonic unwrapped tick value for ``raw_pts``."""
        if not 0 <= raw_pts < self.modulus:
            raise RecoverableError(
                ErrorCode.TIMESTAMP_WRAP,
                f"PTS {raw_pts} outside 33-bit modulus {self.modulus}",
                offset=raw_pts,
            )
        if self._last_raw is None:
            self._last_raw = raw_pts
            self._last_out = raw_pts
            return raw_pts

        delta = raw_pts - self._last_raw
        if delta < -self.half:
            # Forward wrap: e.g. 0x1_FF_FF... -> 0x00...
            self.wraps += 1
        # A large positive delta is ordinary forward progress, not a
        # backward wrap: real frame steps are milliseconds while half the
        # 33-bit modulus is ~4 hours, so only explicit negative jumps that
        # do not cross the modulus boundary remain genuinely backward and
        # are surfaced by classify() instead of mutating the wrap counter.
        self._last_raw = raw_pts
        out = raw_pts + self.wraps * self.modulus
        self._last_out = out
        return out

    def classify(self, raw_pts: int) -> tuple[int, str]:
        """Unwrap and label the step versus the previous sample."""
        prev = self._last_out
        out = self.unwrap(raw_pts)
        if prev is None:
            return out, "initial"
        if out == prev:
            return out, "duplicate"
        if out < prev:
            return out, "backward"
        return out, "advance"


@dataclass(slots=True)
class ClockState:
    """Per-track normalized clock state carried through the pipeline."""

    track_id: str
    time_base: Fraction
    modulus: int = MPEG_PTS_MOD
    unroller: WrapUnroller | None = None
    last_seconds: Fraction | None = None
    frame_count: int = 0

    def __post_init__(self) -> None:
        if self.unroller is None:
            self.unroller = WrapUnroller(self.modulus)

    def normalize(self, raw_pts: int) -> tuple[Fraction, str]:
        """Unwrap a raw PTS and return (exact seconds, step kind)."""
        assert self.unroller is not None
        ticks, kind = self.unroller.classify(raw_pts)
        seconds = ticks_to_seconds(ticks, self.time_base)
        self.last_seconds = seconds
        self.frame_count += 1
        return seconds, kind


@dataclass(slots=True)
class DriftResult:
    """Outcome of a linear drift fit between two clocks."""

    slope: float          # observed/reference; 1.0 means identical rate
    intercept_s: float    # offset at reference time zero
    ppm: float            # (slope - 1) * 1e6
    samples: int
    exceeded: bool
    isolated: bool


class DriftEstimator:
    """Fit ``observed = slope * reference + intercept`` by least squares.

    Inputs are exact rationals; the fit itself is float because ppm is a
    reported quantity, never used to rewrite presentation timestamps.
    """

    def __init__(self, ppm_limit: float) -> None:
        self.ppm_limit = float(ppm_limit)
        self._xs: list[float] = []
        self._ys: list[float] = []

    def add(self, reference_s: Fraction, observed_s: Fraction) -> None:
        self._xs.append(float(reference_s))
        self._ys.append(float(observed_s))

    def fit(self) -> DriftResult | None:
        n = len(self._xs)
        if n < 2:
            return None
        xs = self._xs
        ys = self._ys
        sx = sum(xs)
        sy = sum(ys)
        sxx = sum(x * x for x in xs)
        sxy = sum(x * y for x, y in zip(xs, ys))
        denom = n * sxx - sx * sx
        if denom == 0.0:
            return None
        slope = (n * sxy - sx * sy) / denom
        intercept = (sy - slope * sx) / n
        ppm = (slope - 1.0) * 1_000_000.0
        exceeded = abs(ppm) > self.ppm_limit
        return DriftResult(
            slope=slope,
            intercept_s=intercept,
            ppm=ppm,
            samples=n,
            exceeded=exceeded,
            isolated=False,
        )
