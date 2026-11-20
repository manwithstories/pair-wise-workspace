"""Rational time basis and programme-timeline conversions.

HLS expresses segment positions as integer media sequences plus an optional
``EXT-X-PROGRAM-DATE-TIME``; DASH expresses them as integer ``t`` values on a
manifest-declared timescale.  Both are exact rational numbers, so this module
uses :class:`fractions.Fraction` end to end and only converts to ``float`` at
serialization boundaries.  Floating-point seconds are never used to decide
whether two segments touch or collide -- that is what makes timestamp-rounding
collisions detectable instead of silently corrupting the timeline.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Iterable, Sequence

from .errors import Code, Diagnostic, Locator

MILLISECOND = Fraction(1, 1000)


def rational(value: int | float | Fraction | str) -> Fraction:
    """Promote any supported numeric input to an exact :class:`Fraction`."""
    if isinstance(value, Fraction):
        return value
    if isinstance(value, int):
        return Fraction(value)
    if isinstance(value, float):
        return Fraction(value).limit_denominator(1_000_000_000)
    return Fraction(value)


def seconds(value: int | Fraction, time_base: Fraction) -> Fraction:
    """Convert ``value`` ticks at ``time_base`` seconds/tick to seconds."""
    return rational(value) * rational(time_base)


def ticks_at(seconds_value: Fraction, time_base: Fraction) -> int:
    """Round a time in seconds to the nearest tick of ``time_base``."""
    base = rational(time_base)
    if base <= 0:
        raise ValueError("time_base must be positive")
    return int(round(rational(seconds_value) / base))


def to_millis(seconds_value: Fraction) -> float:
    return float(rational(seconds_value) * 1000)


def to_float_seconds(seconds_value: Fraction) -> float:
    return float(rational(seconds_value))


@dataclass(frozen=True)
class TimelinePoint:
    """One segment's exact position on the programme timeline."""

    sequence: int
    start: Fraction
    duration: Fraction
    representation: str = ""

    @property
    def end(self) -> Fraction:
        return self.start + self.duration

    def to_json(self) -> dict[str, object]:
        return {
            "sequence": self.sequence,
            "representation": self.representation,
            "start_seconds": to_float_seconds(self.start),
            "duration_seconds": to_float_seconds(self.duration),
            "end_seconds": to_float_seconds(self.end),
        }


class TimelinePlanner:
    """Lay segment points onto one programme timeline without silent shifts.

    The planner is fed points in *manifest order* and reports:

    * :data:`Code.REORDERED_REFERENCE` -- a point starts strictly before the
      previous point (the caller may still keep manifest order deliberately),
    * :data:`Code.TIMESTAMP_COLLISION` -- two points round to the same tick at
      the declared time base even though their rational times differ,
    * :data:`Code.TIMESTAMP_DISCONTINUITY` -- a non-empty gap or overlap.

    Nothing is ever dropped or compressed: the output plan keeps one entry per
    input point in sequence order.
    """

    def __init__(self, time_base: Fraction | int = Fraction(1, 90000)) -> None:
        self.time_base = rational(time_base)
        self.points: list[TimelinePoint] = []
        self.diagnostics: list[Diagnostic] = []
        self._tick_seen: dict[int, int] = {}

    def add(
        self,
        sequence: int,
        start: Fraction | int,
        duration: Fraction | int,
        *,
        representation: str = "",
        manifest_path: str = "",
        segment_ref: str = "",
    ) -> TimelinePoint:
        start = rational(start)
        duration = rational(duration)
        point = TimelinePoint(sequence, start, duration, representation)
        locator = Locator(
            manifest_path=manifest_path,
            representation=representation,
            sequence=sequence,
            time_range=(to_float_seconds(start), to_float_seconds(point.end)),
            segment_ref=segment_ref,
        )
        tick = ticks_at(start, self.time_base)
        if tick in self._tick_seen and self._tick_seen[tick] != sequence:
            self.diagnostics.append(
                Diagnostic(
                    Code.TIMESTAMP_COLLISION,
                    "distinct segment starts round to the same time-base tick",
                    locator,
                    {
                        "time_base": str(self.time_base),
                        "tick": tick,
                        "other_sequence": self._tick_seen[tick],
                    },
                )
            )
        else:
            self._tick_seen[tick] = sequence
        if self.points:
            previous = self.points[-1]
            if start < previous.start:
                self.diagnostics.append(
                    Diagnostic(
                        Code.REORDERED_REFERENCE,
                        "segment reference is out of programme-time order",
                        locator,
                        {"previous_sequence": previous.sequence},
                    )
                )
            delta = start - previous.end
            if delta != 0:
                self.diagnostics.append(
                    Diagnostic(
                        Code.TIMESTAMP_DISCONTINUITY,
                        "timeline is not contiguous between adjacent segments",
                        locator,
                        {
                            "gap_seconds": to_float_seconds(delta),
                            "previous_sequence": previous.sequence,
                        },
                    )
                )
        self.points.append(point)
        return point

    @property
    def span(self) -> Fraction:
        if not self.points:
            return Fraction(0)
        return max(p.end for p in self.points) - min(p.start for p in self.points)

    def ordered(self) -> list[TimelinePoint]:
        """Points sorted by (sequence) for consumers that need playback order."""
        return sorted(self.points, key=lambda p: p.sequence)


def within_tolerance(
    measured: Fraction, expected: Fraction, tolerance_ms: float | int
) -> bool:
    """Whether ``|measured - expected|`` is within a millisecond tolerance."""
    tolerance = rational(tolerance_ms) * MILLISECOND
    return abs(rational(measured) - rational(expected)) <= tolerance


def merge_gaps(points: Sequence[TimelinePoint]) -> list[tuple[Fraction, Fraction]]:
    """Return the uncovered timeline ranges (gaps/overlaps merged by sign)."""
    gaps: list[tuple[Fraction, Fraction]] = []
    previous_end: Fraction | None = None
    for point in sorted(points, key=lambda p: p.start):
        if previous_end is not None and point.start > previous_end:
            gaps.append((previous_end, point.start))
        previous_end = point.end if previous_end is None else max(previous_end, point.end)
    return gaps


def total_gap_seconds(points: Iterable[TimelinePoint]) -> Fraction:
    return sum((end - start for start, end in merge_gaps(list(points))), Fraction(0))


__all__ = [
    "MILLISECOND",
    "TimelinePlanner",
    "TimelinePoint",
    "merge_gaps",
    "rational",
    "seconds",
    "ticks_at",
    "to_float_seconds",
    "to_millis",
    "total_gap_seconds",
    "within_tolerance",
]
