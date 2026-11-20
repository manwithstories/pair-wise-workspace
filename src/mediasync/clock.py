"""90 kHz media time base, clock drift estimation, wrap expansion and restarts.

All internal timing uses 90 kHz ticks so audio sample positions and video
presentation times share one integer grid.  Rates are kept as exact fractions
and converted with integer arithmetic wherever possible.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Iterable, Sequence

from .errors import InputError

TICKS_PER_SECOND = 90_000
# Classic MPEG-TS style 33-bit PTS clock.
DEFAULT_WRAP_TICKS = 1 << 33
# A discontinuity at least one second long is treated as an encoder restart.
DEFAULT_RESTART_TICKS = TICKS_PER_SECOND


def ticks_from_seconds(seconds: float) -> int:
    """Convert seconds to 90 kHz ticks, rejecting non-finite input."""

    if seconds != seconds or seconds in (float("inf"), float("-inf")):
        raise InputError(f"non-finite duration: {seconds!r}")
    return int(round(seconds * TICKS_PER_SECOND))


def ticks_to_seconds(ticks: int) -> float:
    return ticks / TICKS_PER_SECOND


def ticks_from_rate(index: int, rate: Fraction) -> int:
    """Exact 90 kHz tick for position ``index`` at ``rate`` Hz."""

    if index < 0:
        raise InputError(f"negative position index: {index}")
    if rate <= 0:
        raise InputError(f"non-positive rate: {rate}")
    return int(Fraction(index, 1) / rate * TICKS_PER_SECOND)


def ticks_from_samples(sample_position: int, sample_rate: int) -> int:
    if sample_rate <= 0:
        raise InputError(f"non-positive sample rate: {sample_rate}")
    if sample_position < 0:
        raise InputError(f"negative sample position: {sample_position}")
    return ticks_from_rate(sample_position, Fraction(sample_rate, 1))


def samples_from_ticks(ticks: int, sample_rate: int) -> int:
    """Nearest whole sample offset for a tick position."""

    if sample_rate <= 0:
        raise InputError(f"non-positive sample rate: {sample_rate}")
    return int(round(Fraction(ticks) * sample_rate / TICKS_PER_SECOND))


def parse_fps(value: str) -> Fraction:
    """Parse ``30``, ``29.97`` or ``30000/1001`` into an exact fraction."""

    text = str(value).strip()
    if not text:
        raise InputError("empty frame rate")
    try:
        if "/" in text:
            numerator, denominator = text.split("/", 1)
            rate = Fraction(int(numerator), int(denominator))
        else:
            rate = Fraction(text)
    except (ValueError, ZeroDivisionError) as exc:
        raise InputError(f"invalid frame rate {value!r}: {exc}") from exc
    if rate <= 0:
        raise InputError(f"frame rate must be positive, got {value!r}")
    return rate


def format_fps(rate: Fraction) -> str:
    if rate.denominator == 1:
        return str(rate.numerator)
    return f"{rate.numerator}/{rate.denominator}"


def wrap_distance(a: int, b: int, wrap: int) -> int:
    """Signed forward distance from ``a`` to ``b`` inside a ``wrap`` clock."""

    if wrap <= 0:
        raise InputError(f"wrap modulus must be positive, got {wrap}")
    return (b - a) % wrap


def unwrap_timestamps(
    values: Sequence[int], wrap: int = DEFAULT_WRAP_TICKS
) -> list[int]:
    """Expand a timestamp sequence across clock wraps.

    A backwards jump larger than half the clock is a wrap; smaller backwards
    jumps are preserved so the caller can classify duplicates, reordering or
    encoder restarts without losing raw provenance.
    """

    if wrap <= 0:
        raise InputError(f"wrap modulus must be positive, got {wrap}")
    out: list[int] = []
    epoch = 0
    previous: int | None = None
    for value in values:
        if not isinstance(value, int) or isinstance(value, bool):
            raise InputError(f"timestamp must be an integer, got {value!r}")
        if not 0 <= value < wrap:
            raise InputError(f"timestamp {value} outside 33-bit range [0, {wrap})")
        if previous is not None and value < previous and previous - value > wrap // 2:
            epoch += 1
        out.append(value + epoch * wrap)
        previous = value
    return out
@dataclass(frozen=True)
class Restart:
    """An encoder restart detected as a large timestamp discontinuity."""

    index: int
    at_ticks: int
    delta_ticks: int
    kind: str

    def to_dict(self) -> dict[str, object]:
        return {
            "index": self.index,
            "at_ticks": self.at_ticks,
            "delta_ticks": self.delta_ticks,
            "kind": self.kind,
        }


def detect_restarts(
    unwrapped: Sequence[int],
    *,
    nominal_step_ticks: int = 1,
    wrap: int = DEFAULT_WRAP_TICKS,
) -> list[Restart]:
    """Flag encoder restarts from timestamp continuity breaks.

    Timestamps arrive in capture order, so any backward jump after wrap
    expansion is a restart.  Forward jumps are only flagged when they dwarf
    the nominal cadence (``max(10 s, 1000 steps)``), which keeps ordinary
    long frame gaps and VFR pauses from being misread.
    """

    if nominal_step_ticks <= 0:
        raise InputError("nominal step must be positive")
    backward_limit = max(1, nominal_step_ticks // 2)
    forward_limit = max(
        max(10 * TICKS_PER_SECOND, 1000 * nominal_step_ticks), wrap // 4
    )
    found: list[Restart] = []
    for index in range(1, len(unwrapped)):
        delta = unwrapped[index] - unwrapped[index - 1]
        if delta <= -backward_limit:
            found.append(Restart(index, unwrapped[index], delta, "backward-jump"))
        elif delta >= forward_limit:
            found.append(Restart(index, unwrapped[index], delta, "forward-gap"))
    return found


@dataclass(frozen=True)
class DriftFit:
    """Affine map from raw capture ticks to reference-timeline ticks.

    ``slope`` is raw ticks per reference tick (capture clock rate).  Display
    time for a raw timestamp is ``origin + (raw - raw0) / slope``.
    """

    origin_ticks: int
    slope: float
    raw0: int
    ref0: int
    intercept: float = 0.0
    rms_ticks: float = 0.0
    max_abs_ticks: int = 0
    samples: int = 0

    @property
    def ppm(self) -> float:
        return (self.slope - 1.0) * 1_000_000.0

    def apply(self, raw_ticks: int) -> int:
        delta = raw_ticks - self.raw0 - self.intercept
        return self.origin_ticks + int(round(delta / self.slope))

    def error_ticks(self, raw_ticks: int, ref_ticks: int) -> int:
        ideal = self.origin_ticks + (ref_ticks - self.ref0)
        return self.apply(raw_ticks) - ideal

    def to_dict(self) -> dict[str, object]:
        return {
            "origin_ticks": self.origin_ticks,
            "slope": self.slope,
            "ppm": self.ppm,
            "raw0": self.raw0,
            "ref0": self.ref0,
            "intercept": self.intercept,
            "rms_ticks": self.rms_ticks,
            "max_abs_ticks": self.max_abs_ticks,
            "samples": self.samples,
        }


def fit_drift(
    raw_ticks: Sequence[int],
    ref_ticks: Sequence[int],
    *,
    origin_ticks: int,
    min_samples: int = 3,
) -> DriftFit:
    """Least-squares fit of raw capture time against reference time."""

    if len(raw_ticks) != len(ref_ticks):
        raise InputError("raw and reference sample counts differ")
    if len(raw_ticks) < min_samples:
        raise InputError(
            f"need at least {min_samples} samples to fit drift, got {len(raw_ticks)}"
        )
    raw0 = raw_ticks[0]
    ref0 = ref_ticks[0]
    xs = [float(r - ref0) for r in ref_ticks]
    ys = [float(r - raw0) for r in raw_ticks]
    n = len(xs)
    mean_x = sum(xs) / n
    mean_y = sum(ys) / n
    sxx = sum((x - mean_x) ** 2 for x in xs)
    if sxx == 0:
        raise InputError("reference timestamps are constant; cannot fit drift")
    sxy = sum((x - mean_x) * (y - mean_y) for x, y in zip(xs, ys))
    slope = sxy / sxx
    if slope <= 0:
        raise InputError(f"fitted clock slope is non-positive: {slope}")
    intercept = mean_y - slope * mean_x
    residuals = [
        origin_ticks + (y - intercept) / slope - (origin_ticks + x)
        for x, y in zip(xs, ys)
    ]
    rms = (sum(r * r for r in residuals) / n) ** 0.5
    max_abs = max((abs(int(round(r))) for r in residuals), default=0)
    return DriftFit(
        origin_ticks=origin_ticks,
        slope=slope,
        raw0=raw0,
        ref0=ref0,
        intercept=intercept,
        rms_ticks=rms,
        max_abs_ticks=max_abs,
        samples=n,
    )
