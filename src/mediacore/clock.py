"""Unified rational time base, timestamp repair and A/V alignment.

A stream's native time base is kept as :class:`fractions.Fraction`, so every
conversion is exact.  Repairs never mutate the observed value: each record keeps
``raw``, ``fixed`` and the human-readable ``reason``.  When the source evidence
allows more than one plausible ordering the record is marked ``uncertain`` and
the interval it may occupy is recorded instead of inventing a value.
"""

from __future__ import annotations

import statistics
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterable, Sequence

from .errors import MediaIssue

#: Default wrap period for 33-bit 90 kHz transport-style counters (2**33 ticks
#: is ~26.5 h; the 32-bit microsecond counter used by MPEG-2 PES is ~71 min).
DEFAULT_WRAP_PERIOD = 2**33
AUDIO_WRAP_PERIOD = 2**32


@dataclass(frozen=True)
class TimeBase:
    """Exact rational time base, e.g. ``1/90000``."""

    num: int = 1
    den: int = 90000

    def __post_init__(self) -> None:
        if self.num <= 0 or self.den <= 0:
            raise ValueError("time base must be positive")

    @classmethod
    def from_ratio(cls, value: Fraction | int | str) -> "TimeBase":
        frac = value if isinstance(value, Fraction) else Fraction(value)
        if frac <= 0:
            raise ValueError("time base must be positive")
        return cls(frac.numerator, frac.denominator)

    def seconds(self, ticks: int | Fraction) -> Fraction:
        return Fraction(ticks) * self.num / self.den

    def ticks(self, seconds: Fraction | float | int) -> int:
        value = Fraction(seconds) * self.den / self.num
        return _round_half_up(value)

    def rescale(self, ticks: int, target: "TimeBase") -> int:
        return _round_half_up(Fraction(ticks) * self.num * target.den / (self.den * target.num))

    @property
    def as_fraction(self) -> Fraction:
        return Fraction(self.num, self.den)

    def __str__(self) -> str:  # pragma: no cover - cosmetic
        return f"{self.num}/{self.den}"


def _round_half_up(value: Fraction) -> int:
    """Round a rational to the nearest integer, halves toward +infinity."""
    if value.denominator == 1:
        return value.numerator
    floor = value.numerator // value.denominator
    frac = value - floor
    return floor + (1 if frac >= Fraction(1, 2) else 0)


def _median_step(ticks: Sequence[int]) -> int:
    steps = [b - a for a, b in zip(ticks, ticks[1:]) if b != a]
    if not steps:
        return 1
    return max(1, int(statistics.median_low(steps)))


@dataclass
class StampDecision:
    """One timestamp after repair, with full provenance.

    ``raw`` is exactly what the container/bitstream delivered (``None`` when the
    value was absent).  ``fixed`` is the value used downstream.  ``reason`` names
    the repair that was applied (``"ok"`` when untouched).  ``uncertain`` marks a
    value inferred from ambiguous evidence; ``window_lo``/``window_hi`` then give
    the tick interval the true value may occupy.
    """

    raw: int | None
    fixed: int
    reason: str
    uncertain: bool = False
    window_lo: int | None = None
    window_hi: int | None = None
    duplicate_of: int | None = None
    note: str = ""

    def to_dict(self, time_base: TimeBase | None = None) -> dict[str, Any]:
        out: dict[str, Any] = {
            "raw": self.raw,
            "fixed": self.fixed,
            "reason": self.reason,
            "uncertain": self.uncertain,
        }
        if time_base is not None:
            out["fixed_seconds"] = float(time_base.seconds(self.fixed))
            if self.raw is not None:
                out["raw_seconds"] = float(time_base.seconds(self.raw))
        if self.window_lo is not None:
            out["window_lo"] = self.window_lo
            out["window_hi"] = self.window_hi
            if time_base is not None:
                out["window_lo_seconds"] = float(time_base.seconds(self.window_lo))
                out["window_hi_seconds"] = float(time_base.seconds(self.window_hi))
        if self.duplicate_of is not None:
            out["duplicate_of"] = self.duplicate_of
        if self.note:
            out["note"] = self.note
        return out


@dataclass
class TimelineRepair:
    """Result of mapping one stream's timestamps to a monotonic timeline."""

    time_base: TimeBase
    decisions: list[StampDecision]
    issues: list[MediaIssue] = field(default_factory=list)
    wrap_count: int = 0
    unit_ticks: int = 1

    @property
    def uncertain_count(self) -> int:
        return sum(1 for d in self.decisions if d.uncertain)

    def fixed_ticks(self) -> list[int]:
        return [d.fixed for d in self.decisions]

    def fixed_seconds(self) -> list[Fraction]:
        return [self.time_base.seconds(d.fixed) for d in self.decisions]

    def summary(self) -> dict[str, Any]:
        counts: dict[str, int] = {}
        for d in self.decisions:
            counts[d.reason] = counts.get(d.reason, 0) + 1
        return {
            "records": len(self.decisions),
            "wrap_count": self.wrap_count,
            "uncertain": self.uncertain_count,
            "repairs": counts,
            "unit_ticks": self.unit_ticks,
            "issues": [i.to_dict() for i in self.issues],
        }


def _round_half_up(value: Fraction) -> int:
    """Round a rational to the nearest integer, halves toward +infinity."""
    if value.denominator == 1:
        return value.numerator
    floor = value.numerator // value.denominator
    frac = value - floor
    return floor + (1 if frac >= Fraction(1, 2) else 0)


def _median_step(ticks: Sequence[int]) -> int:
    steps = [b - a for a, b in zip(ticks, ticks[1:]) if b != a]
    if not steps:
        return 1
    return max(1, int(statistics.median_low(steps)))
