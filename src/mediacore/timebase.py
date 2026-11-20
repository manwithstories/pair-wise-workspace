"""Unified microsecond timebase and rounding rules.

Every timestamp that crosses a module boundary in mediacore is an integer
number of microseconds (``Us``).  Integers are the whole point: float seconds
accumulate error over a long timeline and silently drift clip boundaries.
Converting from a container's rational timebase therefore happens exactly
once, here, using :class:`fractions.Fraction` arithmetic and one explicit
rounding rule per direction.

Rounding rules (documented because tests assert them):

``ticks_to_us``
    Nearest integer microsecond, ties away from zero.  We never truncate: a
    truncated timestamp walks backwards once per packet, which over 10 minutes
    at 30 fps is measurable drift.

``us_to_ticks``
    Nearest integer tick with the same tie rule, but *clamped* to the tick
    grid so that ``us_to_ticks(ticks_to_us(t))`` is stable for every tick.

``floor_us`` / ``ceil_us``
    Used only where a half-sample must belong to exactly one clip.

The :class:`DriftMeter` records the per-conversion rounding residual so the
report can publish ``timeline_drift_us`` — the worst absolute error observed
between the exact rational position and the integer microsecond we stored.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Iterable

from .errors import ParameterError

__all__ = [
    "US_PER_SECOND",
    "NS_PER_US",
    "Us",
    "Timebase",
    "DriftMeter",
    "round_half_away",
    "ticks_to_us",
    "us_to_ticks",
    "seconds_to_us",
    "us_to_seconds",
    "frame_index",
    "frame_index_exact",
    "samples_for_duration",
]

US_PER_SECOND = 1_000_000
NS_PER_US = 1_000

Us = int  # type alias, purely documentary


def round_half_away(value: Fraction) -> int:
    """Round a Fraction to the nearest integer, ties away from zero.

    ``int(Fraction)`` truncates toward zero, so we first add a signed half.
    """
    if value >= 0:
        return int(value + Fraction(1, 2))
    return -int(-value + Fraction(1, 2))


_round = round_half_away


@dataclass(frozen=True)
class Timebase:
    """A rational timebase ``num/den`` seconds per tick (e.g. 1/15360)."""

    num: int
    den: int

    def __post_init__(self) -> None:
        if self.num <= 0 or self.den <= 0:
            raise ParameterError(
                "timebase must be positive", timebase=f"{self.num}/{self.den}"
            )

    @classmethod
    def parse(cls, text: str | Fraction | tuple[int, int] | "Timebase") -> "Timebase":
        """Accept ``"1/15360"``, ``"15360"`` (as 1/N), Fraction or tuple."""
        if isinstance(text, Timebase):
            return text
        if isinstance(text, Fraction):
            return cls(text.numerator, text.denominator)
        if isinstance(text, tuple):
            return cls(int(text[0]), int(text[1]))
        text = str(text).strip()
        try:
            if "/" in text:
                num, den = text.split("/", 1)
                return cls(int(num), int(den))
            # av exposes inverse timebases as plain ints in some places
            return cls(1, int(text))
        except (ValueError, ZeroDivisionError) as exc:
            raise ParameterError("malformed timebase", timebase=text) from exc

    @property
    def fraction(self) -> Fraction:
        return Fraction(self.num, self.den)

    @property
    def us_per_tick(self) -> Fraction:
        return Fraction(US_PER_SECOND * self.num, self.den)

    def to_us(self, ticks: int) -> Us:
        """Exact tick -> microsecond conversion with documented rounding."""
        return _round(Fraction(ticks) * self.us_per_tick)

    def to_us_fraction(self, ticks: int) -> Fraction:
        """The *unrounded* microsecond position (for drift measurement)."""
        return Fraction(ticks) * self.us_per_tick

    def from_us(self, us: Us) -> int:
        """Microseconds -> ticks, nearest tick (ties away from zero)."""
        return _round(Fraction(us) * self.den / (US_PER_SECOND * self.num))

    def from_us_floor(self, us: Us) -> int:
        return _floor(Fraction(us) * self.den / (US_PER_SECOND * self.num))

    def from_us_ceil(self, us: Us) -> int:
        return _ceil(Fraction(us) * self.den / (US_PER_SECOND * self.num))

    def ticks_per_second(self) -> Fraction:
        return Fraction(self.den, self.num)

    def __str__(self) -> str:  # pragma: no cover - cosmetic
        return f"{self.num}/{self.den}"


def _floor(value: Fraction) -> int:
    return value.numerator // value.denominator


def _ceil(value: Fraction) -> int:
    return -((-value.numerator) // value.denominator)


def ticks_to_us(ticks: int, tb: Timebase) -> Us:
    return tb.to_us(ticks)


def us_to_ticks(us: Us, tb: Timebase) -> int:
    return tb.from_us(us)


def seconds_to_us(seconds: float | int | str | Fraction) -> Us:
    """Convert seconds (float, str or Fraction) to integer microseconds."""
    if isinstance(seconds, str):
        seconds = Fraction(seconds)
    if isinstance(seconds, int):
        return seconds * US_PER_SECOND
    if isinstance(seconds, Fraction):
        return _round(seconds * US_PER_SECOND)
    return _round(Fraction(seconds).limit_denominator(1_000_000_000) * US_PER_SECOND)


def us_to_seconds(us: Us) -> Fraction:
    """Exact (Fraction) seconds for reporting; never use for comparisons."""
    return Fraction(us, US_PER_SECOND)


@dataclass
class DriftMeter:
    """Tracks the worst residual introduced by us rounding.

    ``timeline_drift_us`` in the report is exactly ``max_abs_us`` rounded up to
    the next whole microsecond (i.e. ``ceil``), so a residual of 0.4 us is
    reported as 1 us and a perfect conversion as 0.
    """

    count: int = 0
    max_abs: Fraction = Fraction(0)
    total_abs: Fraction = Fraction(0)
    worst_at: int | None = None

    def record(self, exact_us: Fraction, stored_us: int, at: int | None = None) -> None:
        residual = exact_us - stored_us
        mag = abs(residual)
        self.count += 1
        self.total_abs += mag
        if mag > self.max_abs:
            self.max_abs = mag
            self.worst_at = at if at is not None else stored_us

    def merge(self, other: "DriftMeter") -> None:
        self.count += other.count
        self.total_abs += other.total_abs
        if other.max_abs > self.max_abs:
            self.max_abs = other.max_abs
            self.worst_at = other.worst_at

    @property
    def max_abs_us(self) -> int:
        return _ceil(self.max_abs)

    @property
    def mean_abs_us(self) -> float:
        if not self.count:
            return 0.0
        return float(self.total_abs / self.count)

    def to_dict(self) -> dict[str, float | int]:
        return {
            "conversions": self.count,
            "max_abs_us": self.max_abs_us,
            "mean_abs_us": round(self.mean_abs_us, 6),
            "worst_at": self.worst_at if self.worst_at is not None else 0,
        }


def frame_index(position_us: Us, frame_duration_us: int) -> int:
    """Nearest frame index for a position, ties toward later frame."""
    if frame_duration_us <= 0:
        raise ParameterError("frame duration must be positive", frame_duration_us=frame_duration_us)
    return _round(Fraction(position_us, frame_duration_us))


def frame_index_exact(position_us: Us, frame_duration_us: int) -> Fraction:
    return Fraction(position_us, frame_duration_us)


def samples_for_duration(duration_us: Us, sample_rate: int) -> int:
    """Deterministically map a duration to an integer sample count.

    Nearest sample, ties away from zero, so a clip boundary always yields the
    same number of samples regardless of how the audio was chunked upstream.
    """
    if sample_rate <= 0:
        raise ParameterError("sample rate must be positive", sample_rate=sample_rate)
    return _round(Fraction(duration_us * sample_rate, US_PER_SECOND))


def align_us(
    raw_ticks: Iterable[int],
    tb: Timebase,
    *,
    origin_us: Us = 0,
    meter: DriftMeter | None = None,
    wrap_at: int | None = None,
) -> list[Us]:
    """Convert a tick sequence to micros, rebasing onto ``origin_us``.

    Handles, in this order:

    * **wrap**: a timestamp that jumps backwards by more than half the
      ``wrap_at`` modulus is treated as counter wraparound and unwrapped.
    * **origin**: ``origin_us`` is subtracted so negative or non-zero start
      tracks land on the unified timeline.
    * **rounding**: every conversion is recorded in ``meter``.

    Duplicate and missing timestamps are *not* removed here; they are
    reported by :mod:`mediacore.probe`, because dropping them silently would
    change clip semantics.
    """
    out: list[Us] = []
    prev_raw: int | None = None
    unwrap = 0
    for raw in raw_ticks:
        adjusted = raw
        if wrap_at is not None and prev_raw is not None:
            modulus = wrap_at
            delta = adjusted - prev_raw
            if delta < -modulus // 2:
                unwrap += modulus
            elif delta > modulus // 2:
                unwrap -= modulus
            adjusted = raw + unwrap
        exact = tb.to_us_fraction(adjusted) - origin_us
        stored = _round(exact)
        if meter is not None:
            meter.record(exact, stored, at=raw)
        out.append(stored)
        prev_raw = adjusted
    return out
