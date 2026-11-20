"""Exact rational media time.

All timeline arithmetic in bcastqc is performed on :class:`fractions.Fraction`
values or on integer tick counts; floating point is only ever a *presentation*
format.  This module owns the conversion rules so that every other module
(time-line, decode, render, report) uses one consistent clock.

Concepts
--------
``TimeBase``
    A positive rational number of seconds per tick, e.g. ``90000/1`` ticks per
    second is ``TimeBase(1, 90000)``.  Tick counts are plain integers.

``MediaTime``
    An integer tick count paired with a ``TimeBase``.  Addition and subtraction
    require matching bases; conversion between bases is explicit and applies a
    documented rounding mode.

``round_rational``
    Converts an exact :class:`Fraction` to an integer using a rounding mode.
    ``HALF_EVEN`` is the library default because repeated rescaling must not
    accumulate bias in either direction.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from fractions import Fraction
from typing import Iterable

__all__ = [
    "Rounding",
    "round_rational",
    "TimeBase",
    "MediaTime",
    "MediaWindow",
    "AUDIO_SAMPLE_BASE",
    "seconds_to_base",
]


class Rounding(str, Enum):
    """Rounding modes for rational-to-integer conversion."""

    FLOOR = "floor"
    CEIL = "ceil"
    TRUNC = "trunc"
    HALF_EVEN = "half_even"
    HALF_UP = "half_up"


def round_rational(value: Fraction | int, mode: Rounding) -> int:
    """Round an exact rational to the nearest integer under ``mode``.

    ``HALF_UP`` rounds halves away from zero; ``HALF_EVEN`` rounds halves to
    the even neighbour (banker's rounding).  ``TRUNC`` rounds toward zero.
    """
    if isinstance(value, int):
        return value
    if not isinstance(value, Fraction):  # Fraction(int) is fine; anything else is a bug
        value = Fraction(value)
    num, den = value.numerator, value.denominator
    if den == 1:
        return num
    floor = num // den
    if mode is Rounding.FLOOR:
        return floor
    if mode is Rounding.CEIL:
        return floor if num % den == 0 else floor + 1
    if mode is Rounding.TRUNC:
        return floor if num >= 0 else -((-num) // den)
    remainder = num - floor * den
    twice = remainder * 2
    if twice < den:
        return floor
    if twice > den:
        return floor + 1
    # exact half
    if mode is Rounding.HALF_UP:
        return floor + 1 if value >= 0 else floor
    # HALF_EVEN: choose the even neighbour
    return floor if floor % 2 == 0 else floor + 1


@dataclass(frozen=True, order=False)
class TimeBase:
    """Seconds per tick, stored as a reduced positive rational."""

    numerator: int
    denominator: int = 1

    def __post_init__(self) -> None:
        if not isinstance(self.numerator, int) or not isinstance(self.denominator, int):
            raise TypeError("TimeBase components must be integers")
        if self.numerator <= 0 or self.denominator <= 0:
            raise ValueError("TimeBase must be positive")
        frac = Fraction(self.numerator, self.denominator)
        object.__setattr__(self, "numerator", frac.numerator)
        object.__setattr__(self, "denominator", frac.denominator)

    @classmethod
    def parse(cls, text: str) -> "TimeBase":
        """Parse ``"num/den"`` or a bare integer (meaning ``n/1``)."""
        text = text.strip()
        if "/" in text:
            num, den = text.split("/", 1)
            return cls(int(num), int(den))
        return cls(int(text), 1)

    @property
    def seconds_per_tick(self) -> Fraction:
        return Fraction(self.numerator, self.denominator)

    @property
    def ticks_per_second(self) -> Fraction:
        return Fraction(self.denominator, self.numerator)

    def rescale(self, ticks: int, target: "TimeBase", mode: Rounding = Rounding.HALF_EVEN) -> int:
        """Convert ``ticks`` from this base into ``target`` ticks, exactly."""
        if target == self:
            return ticks
        return round_rational(Fraction(ticks) * self.seconds_per_tick / target.seconds_per_tick, mode)

    def __str__(self) -> str:
        return f"{self.numerator}/{self.denominator}"

    def to_json(self) -> str:
        return str(self)


# Audio is measured in integer samples; 1/48000 is only a default convenience.
AUDIO_SAMPLE_BASE = TimeBase(1, 48000)
VIDEO_TICK_BASE = TimeBase(1, 90000)


def seconds_to_base(seconds: Fraction | int | str, base: TimeBase) -> int:
    """Convert a duration in seconds to whole ticks of ``base`` (half-even)."""
    if isinstance(seconds, str):
        seconds = Fraction(seconds)
    return round_rational(Fraction(seconds) / base.seconds_per_tick, Rounding.HALF_EVEN)


@dataclass(frozen=True)
class MediaTime:
    """An exact instant (or duration) in a fixed time base."""

    ticks: int
    base: TimeBase

    def __post_init__(self) -> None:
        if not isinstance(self.ticks, int):
            raise TypeError("MediaTime.ticks must be an integer")

    def to_base(self, target: TimeBase, mode: Rounding = Rounding.HALF_EVEN) -> "MediaTime":
        return MediaTime(self.base.rescale(self.ticks, target, mode), target)

    def seconds(self) -> Fraction:
        return Fraction(self.ticks) * self.base.seconds_per_tick

    def milliseconds(self) -> Fraction:
        return self.seconds() * 1000

    def __add__(self, other: "MediaTime") -> "MediaTime":
        self._require_same_base(other)
        return MediaTime(self.ticks + other.ticks, self.base)

    def __sub__(self, other: "MediaTime") -> "MediaTime":
        self._require_same_base(other)
        return MediaTime(self.ticks - other.ticks, self.base)

    def __lt__(self, other: "MediaTime") -> bool:
        return self._compare(other) < 0

    def __le__(self, other: "MediaTime") -> bool:
        return self._compare(other) <= 0

    def __gt__(self, other: "MediaTime") -> bool:
        return self._compare(other) > 0

    def __ge__(self, other: "MediaTime") -> bool:
        return self._compare(other) >= 0

    def _compare(self, other: "MediaTime") -> int:
        if not isinstance(other, MediaTime):
            return NotImplemented
        left = self.seconds()
        right = other.seconds()
        return (left > right) - (left < right)

    def _require_same_base(self, other: "MediaTime") -> None:
        if not isinstance(other, MediaTime):
            raise TypeError("expected MediaTime")
        if other.base != self.base:
            raise ValueError(f"time base mismatch: {self.base} vs {other.base}")

    def __str__(self) -> str:
        return f"{self.ticks}@{self.base}"

    def to_json(self) -> dict:
        return {"ticks": self.ticks, "time_base": str(self.base)}


@dataclass(frozen=True)
class MediaWindow:
    """A half-open interval ``[start, start + duration)`` in one time base."""

    start: MediaTime
    duration: MediaTime

    def __post_init__(self) -> None:
        if self.start.base != self.duration.base:
            raise ValueError("window start and duration must share a time base")
        if self.duration.ticks < 0:
            raise ValueError("window duration must be non-negative")

    @property
    def base(self) -> TimeBase:
        return self.start.base

    @property
    def end(self) -> MediaTime:
        return MediaTime(self.start.ticks + self.duration.ticks, self.base)

    def to_base(self, target: TimeBase, mode: Rounding = Rounding.HALF_EVEN) -> "MediaWindow":
        return MediaWindow(self.start.to_base(target, mode), self.duration.to_base(target, mode))

    def contains(self, moment: MediaTime) -> bool:
        return self.start <= moment < self.end

    def intersect(self, other: "MediaWindow") -> "MediaWindow | None":
        self._require_comparable(other)
        lo = max(self.start, other.start)
        hi = min(self.end, other.end)
        if hi <= lo:
            return None
        return MediaWindow(lo, MediaTime(hi.ticks - lo.ticks, self.base))

    def union(self, other: "MediaWindow") -> "MediaWindow":
        self._require_comparable(other)
        lo = min(self.start, other.start)
        hi = max(self.end, other.end)
        return MediaWindow(lo, MediaTime(hi.ticks - lo.ticks, self.base))

    def overlaps(self, other: "MediaWindow") -> bool:
        return self.intersect(other) is not None

    def _require_comparable(self, other: "MediaWindow") -> None:
        if not isinstance(other, MediaWindow):
            raise TypeError("expected MediaWindow")
        if other.base != self.base:
            raise ValueError(f"time base mismatch: {self.base} vs {other.base}")

    def to_seconds(self) -> tuple[Fraction, Fraction]:
        return self.start.seconds(), self.end.seconds()

    def to_json(self) -> dict:
        return {"start_ticks": self.start.ticks, "duration_ticks": self.duration.ticks, "time_base": str(self.base)}


def merge_windows(windows: Iterable[MediaWindow]) -> list[MediaWindow]:
    """Merge overlapping or adjacent windows, preserving order of first start."""
    items = sorted(windows, key=lambda w: (w.start.seconds(), w.end.seconds()))
    merged: list[MediaWindow] = []
    for window in items:
        if merged and merged[-1].end >= window.start:
            merged[-1] = merged[-1].union(window)
        else:
            merged.append(window)
    return merged
