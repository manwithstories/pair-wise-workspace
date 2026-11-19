"""Unified rational media time base for every relay diagnosis stage.

All timestamps inside relaydiag are carried as exact rational numbers so that
probe, clock unwrapping, damage mapping, checkpoints and the manifest never
disagree because of floating point rounding.  Wall-clock seconds are derived
from a rational timestamp only at presentation time.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction

#: Sentinel PTS value used by the generator for missing timestamps.
NO_PTS = 1 << 63


class TimeBase:
    """A fixed tick rate expressed as ``num/den`` seconds per tick."""

    __slots__ = ("num", "den")

    def __init__(self, num: int, den: int) -> None:
        if num <= 0 or den <= 0:
            raise ValueError(f"time base components must be positive: {num}/{den}")
        self.num = num
        self.den = den

    @classmethod
    def from_fraction(cls, frac: Fraction) -> "TimeBase":
        return cls(frac.numerator, frac.denominator)

    def fraction(self) -> Fraction:
        return Fraction(self.num, self.den)

    def ticks_to_seconds(self, ticks: int) -> Fraction:
        return Fraction(ticks * self.num, self.den)

    def seconds_to_ticks(self, seconds: Fraction | float) -> int:
        return int(round(Fraction(seconds) / self.fraction()))

    def to(self, ticks: int, other: "TimeBase") -> int:
        """Exact rounded conversion of ticks between two time bases."""
        value = Fraction(ticks) * self.fraction() / other.fraction()
        # Banker-free half-up rounding keeps monotonic ordering stable.
        return int(value + (Fraction(1, 2) if value >= 0 else Fraction(-1, 2)))

    def __eq__(self, other: object) -> bool:
        return isinstance(other, TimeBase) and self.fraction() == other.fraction()

    def __hash__(self) -> int:
        return hash(self.fraction())

    def __repr__(self) -> str:
        return f"TimeBase({self.num}/{self.den})"


@dataclass(frozen=True, slots=True)
class Timestamp:
    """A rational instant on a named track timeline."""

    ticks: int
    base: TimeBase
    track: str = "ref"

    @property
    def seconds(self) -> Fraction:
        return self.base.ticks_to_seconds(self.ticks)

    @property
    def milliseconds(self) -> float:
        return float(self.seconds * 1000)

    def on(self, base: TimeBase, track: str | None = None) -> "Timestamp":
        return Timestamp(self.base.to(self.ticks, base), base, track or self.track)

    def __sub__(self, other: "Timestamp") -> Fraction:
        if not isinstance(other, Timestamp):
            return NotImplemented
        return self.seconds - other.seconds


@dataclass(frozen=True, slots=True)
class TimeRange:
    """Half-open rational interval ``[start, end)`` on a track timeline."""

    start: Fraction
    end: Fraction
    track: str = "ref"

    def __post_init__(self) -> None:
        if self.end < self.start:
            raise ValueError(f"inverted time range: {self.end} < {self.start}")

    @property
    def duration(self) -> Fraction:
        return self.end - self.start

    @property
    def center(self) -> Fraction:
        return (self.start + self.end) / 2

    def overlaps(self, other: "TimeRange") -> bool:
        return self.start < other.end and other.start < self.end

    def to_dict(self) -> dict:
        return {
            "track": self.track,
            "start_s": float(self.start),
            "end_s": float(self.end),
            "duration_ms": float(self.duration * 1000),
        }


def common_base(*bases: TimeBase) -> TimeBase:
    """Pick a time base that is an exact multiple of every input base."""
    if not bases:
        raise ValueError("common_base requires at least one time base")
    rate = Fraction(1)
    for base in bases:
        rate = max(rate, Fraction(base.den, base.num))
    # A 1/rate-second tick divides all input tick grids without rounding.
    frac = Fraction(1, rate)
    return TimeBase(frac.numerator, frac.denominator)


def monotonic(values: list[int]) -> bool:
    """True when every value is strictly greater than the previous one."""
    return all(b > a for a, b in zip(values, values[1:]))


def wrap_delta(value: int, previous: int, modulus: int) -> int:
    """Signed shortest-path delta from ``previous`` to ``value`` on a ring.

    Used both for 32-bit PTS wraparound and for generic modular counters.
    """
    half = modulus // 2
    delta = (value - previous) % modulus
    if delta > half:
        delta -= modulus
    return delta
