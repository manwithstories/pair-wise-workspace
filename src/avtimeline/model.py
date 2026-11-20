"""Rational time base and media clock arithmetic.

Every timestamp in the project is an exact :class:`Fraction` of seconds.  The
helpers here convert between a media time base, integer sample counts and
display strings without ever falling back to binary floating point.  Floating
point is only used for human-facing diagnostics, never for comparisons.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Iterable

# One frame or one audio block is the finest unit the acceptance criteria care
# about.  The helpers below expose it as an exact error bound.
DEFAULT_ERROR_TOLERANCE = Fraction(1, 1000)


class TimeError(ValueError):
    """Raised when a timestamp cannot be represented exactly."""


def as_fraction(value: object) -> Fraction:
    """Convert *value* to an exact rational number of seconds.

    ``int``/``Fraction`` are accepted directly; ``float`` values are rejected
    because they silently carry unrelated binary noise into comparisons.
    """
    if isinstance(value, Fraction):
        return value
    if isinstance(value, bool):
        raise TimeError("boolean is not a valid time value")
    if isinstance(value, int):
        return Fraction(value)
    if isinstance(value, str):
        try:
            return Fraction(value)
        except (ValueError, ZeroDivisionError) as exc:  # pragma: no cover - defensive
            raise TimeError(f"invalid rational time string: {value!r}") from exc
    raise TimeError(f"unsupported time value: {value!r} of type {type(value).__name__}")


def fraction_from_float(value: float) -> Fraction:
    """Convert a float-origin timestamp using its shortest decimal repr.

    This is the only sanctioned float path: it keeps e.g. ``0.1`` as ``1/10``
    instead of the binary expansion, so values that originate in JSON or user
    input stay readable and stable across runs.
    """
    if value != value or value in (float("inf"), float("-inf")):
        raise TimeError(f"non-finite time value: {value!r}")
    return Fraction(repr(float(value)))


def quantize(value: Fraction, step: Fraction) -> int:
    """Return how many whole *step* units fit into *value* (rounding down)."""
    if step <= 0:
        raise TimeError("quantization step must be positive")
    return int(value / step)


def snap(value: Fraction, step: Fraction) -> Fraction:
    """Round *value* to the nearest multiple of *step*, ties toward zero."""
    if step <= 0:
        raise TimeError("quantization step must be positive")
    units = value / step
    lower = units.numerator // units.denominator
    remainder = units - lower
    if remainder > Fraction(1, 2):
        lower += 1
    elif remainder == Fraction(1, 2) and value < 0:
        lower += 1
    return lower * step


@dataclass(frozen=True, order=True)
class Rate:
    """A rational media rate such as ``30000/1001``."""

    numerator: int
    denominator: int = 1

    def __post_init__(self) -> None:
        if self.denominator == 0:
            raise TimeError("rate denominator must not be zero")
        if self.numerator <= 0:
            raise TimeError("rate numerator must be positive")

    @classmethod
    def parse(cls, text: str) -> "Rate":
        if "/" in text:
            num, _, den = text.partition("/")
            return cls(int(num), int(den))
        return cls(int(text))

    @property
    def value(self) -> Fraction:
        return Fraction(self.numerator, self.denominator)

    @property
    def frame_duration(self) -> Fraction:
        """Exact duration of one frame at this rate."""
        return 1 / self.value

    def __str__(self) -> str:  # pragma: no cover - trivial
        if self.denominator == 1:
            return str(self.numerator)
        return f"{self.numerator}/{self.denominator}"


@dataclass(frozen=True)
class TimeBase:
    """A media time base expressed as seconds per tick, e.g. ``1/48000``."""

    ticks_per_second: int

    def __post_init__(self) -> None:
        if self.ticks_per_second <= 0:
            raise TimeError("time base must be positive")

    def to_fraction(self, ticks: int) -> Fraction:
        return Fraction(int(ticks), self.ticks_per_second)

    def from_seconds(self, seconds: Fraction) -> Fraction:
        """Seconds expressed in ticks; may be fractional, callers decide."""
        return seconds * self.ticks_per_second

    def floor_ticks(self, seconds: Fraction) -> int:
        units = seconds * self.ticks_per_second
        return units.numerator // units.denominator

    def ceil_ticks(self, seconds: Fraction) -> int:
        units = seconds * self.ticks_per_second
        return -((-units.numerator) // units.denominator)


@dataclass(frozen=True, order=True)
class Span:
    """A half-open interval ``[start, end)`` on the shared clock."""

    start: Fraction
    end: Fraction

    def __post_init__(self) -> None:
        if not isinstance(self.start, Fraction) or not isinstance(self.end, Fraction):
            raise TimeError("span bounds must be exact fractions")
        if self.end < self.start:
            raise TimeError(f"span end {self.end} precedes start {self.start}")

    @classmethod
    def of(cls, start: object, end: object) -> "Span":
        return cls(as_fraction(start), as_fraction(end))

    @property
    def duration(self) -> Fraction:
        return self.end - self.start

    @property
    def is_empty(self) -> bool:
        return self.end == self.start

    def overlaps(self, other: "Span") -> bool:
        return self.start < other.end and other.start < self.end

    def touches(self, other: "Span") -> bool:
        return self.end == other.start or other.end == self.start

    def contains(self, moment: Fraction) -> bool:
        return self.start <= moment < self.end

    def intersection(self, other: "Span") -> "Span | None":
        start = max(self.start, other.start)
        end = min(self.end, other.end)
        if end <= start:
            return None
        return Span(start, end)

    def union(self, other: "Span") -> "Span":
        return Span(min(self.start, other.start), max(self.end, other.end))

    def shift(self, delta: Fraction) -> "Span":
        return Span(self.start + delta, self.end + delta)


def merge_spans(spans: Iterable[Span]) -> list[Span]:
    """Return the sorted union of a span iterable.

    Empty spans are ignored; touching spans are merged because the timeline
    treats ``[0, 1)`` followed by ``[1, 2)`` as continuous coverage.
    """
    ordered = sorted((s for s in spans if not s.is_empty), key=lambda s: (s.start, s.end))
    merged: list[Span] = []
    for span in ordered:
        if merged and span.start <= merged[-1].end:
            last = merged[-1]
            if span.end > last.end:
                merged[-1] = Span(last.start, span.end)
        else:
            merged.append(span)
    return merged


def gaps_within(coverage: Iterable[Span], window: Span) -> list[Span]:
    """Return the gaps of *coverage* restricted to *window*."""
    merged = merge_spans(coverage)
    gaps: list[Span] = []
    cursor = window.start
    for span in merged:
        clipped = span.intersection(window)
        if clipped is None:
            continue
        if clipped.start > cursor:
            gaps.append(Span(cursor, clipped.start))
        cursor = max(cursor, clipped.end)
    if cursor < window.end:
        gaps.append(Span(cursor, window.end))
    return gaps


def overlaps_any(coverage: Iterable[Span], window: Span) -> list[Span]:
    """Return the covering spans that intersect *window*."""
    result = []
    for span in coverage:
        clipped = span.intersection(window)
        if clipped is not None:
            result.append(clipped)
    return result


def format_time(value: Fraction) -> str:
    """Render a timestamp as ``HH:MM:SS.mmm`` for diagnostics."""
    sign = "-" if value < 0 else ""
    millis_total = int(round(abs(value) * 1000))
    hours, rem = divmod(millis_total, 3_600_000)
    minutes, rem = divmod(rem, 60_000)
    seconds, millis = divmod(rem, 1000)
    return f"{sign}{hours:02d}:{minutes:02d}:{seconds:02d}.{millis:03d}"


def format_rational(value: Fraction) -> str:
    """Render an exact rational for JSON output (keeps full precision)."""
    if value.denominator == 1:
        return str(value.numerator)
    return f"{value.numerator}/{value.denominator}"


def seconds_to_float(value: Fraction) -> float:
    """Human-facing float; never use the result for equality checks."""
    return float(value)
