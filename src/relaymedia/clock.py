"""Unified rational clock.

Every time value in relaymedia is a :class:`fractions.Fraction` of a second on
one unified timeline. Floats never drive boundary decisions, so the same input
always reproduces the same sample indices.

Supported editorial timecode features:

* fractional frame rates (``30000/1001``, ``29.97``, ``24`` ...);
* negative timeline offsets, so a source whose timecode starts before zero is
  still mapped honestly instead of being silently shifted;
* day-boundary wrap: a stamp that goes backwards (``23:59:59:29`` then
  ``00:00:00:00``) gains exactly one day, tracked by the clock instance;
* repeated stamps: two identical stamps map to the same instant and are not an
  error;
* unconvertible records raise :class:`relaymedia.errors.InputError` carrying
  the source byte position and are collected by callers — boundaries are never
  moved to hide them.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path

from .errors import ErrorRecord, InputError, Stage

_SECONDS_PER_DAY = 24 * 3600


def parse_rate(text: str) -> Fraction:
    """Parse ``"30000/1001"``, ``"29.97"`` or ``"24"`` into a frame rate."""
    text = text.strip()
    if not text:
        raise InputError("empty frame rate", stage=Stage.TIMELINE)
    try:
        rate = Fraction(text)
    except (ValueError, ZeroDivisionError) as exc:
        raise InputError(f"unparseable frame rate {text!r}", stage=Stage.TIMELINE) from exc
    if rate <= 0:
        raise InputError(f"frame rate must be positive, got {text!r}", stage=Stage.TIMELINE)
    return rate


@dataclass(frozen=True, slots=True)
class Stamp:
    """One raw editorial timecode ``hh:mm:ss:ff`` with its source position."""

    hour: int
    minute: int
    second: int
    frame: int
    raw: str
    offset_bytes: int = -1
    line: int = -1


def parse_timecode_stamp(text: str, *, offset_bytes: int = -1, line: int = -1) -> Stamp:
    """Parse ``hh:mm:ss:ff``; malformed input raises a located InputError."""
    parts = text.strip().split(":")
    if len(parts) != 4:
        raise InputError(
            f"timecode must be hh:mm:ss:ff, got {text!r}",
            stage=Stage.TIMELINE,
        )
    try:
        hour, minute, second, frame = (int(p) for p in parts)
    except ValueError as exc:
        raise InputError(f"non-numeric timecode field in {text!r}", stage=Stage.TIMELINE) from exc
    if hour < 0 or minute < 0 or second < 0 or frame < 0:
        raise InputError(f"negative field in timecode {text!r}", stage=Stage.TIMELINE)
    if minute >= 60 or second >= 60:
        raise InputError(f"minute/second out of range in timecode {text!r}", stage=Stage.TIMELINE)
    return Stamp(hour, minute, second, frame, text.strip(), offset_bytes, line)


def parse_clock_time(text: str, *, sep: str, offset_bytes: int = -1, line: int = -1) -> Fraction:
    """Parse an ``hh:mm:ss`` fractional clock (SRT ``,`` or VTT ``.`` sep)."""
    head, found, fraction = text.strip().partition(sep)
    if not found:
        raise InputError(f"clock time missing {sep!r} separator: {text!r}", stage=Stage.SUBTITLE)
    fields = head.split(":")
    if len(fields) != 3 or any(not f.isdigit() for f in fields):
        raise InputError(f"clock time must be hh:mm:ss, got {text!r}", stage=Stage.SUBTITLE)
    if not fraction.isdigit():
        raise InputError(f"fractional seconds must be digits, got {text!r}", stage=Stage.SUBTITLE)
    hour, minute, second = (int(f) for f in fields)
    if minute >= 60 or second >= 60:
        raise InputError(f"clock time field out of range: {text!r}", stage=Stage.SUBTITLE)
    return Fraction(hour * 3600 + minute * 60 + second) + Fraction(int(fraction), 10 ** len(fraction))


class TimecodeClock:
    """Stateful converter from raw frame stamps to unified timeline seconds."""

    def __init__(self, rate: Fraction, offset_seconds: Fraction | int = 0) -> None:
        if rate <= 0:
            raise InputError("frame rate must be positive", stage=Stage.TIMELINE)
        self.rate = rate
        # Frame digits count: 30 for 29.97, 60 for 59.94, rate itself when integral.
        self.frames_per_second = int(round(rate))
        self.offset = Fraction(offset_seconds)
        self._wraps = 0
        self._last_raw: Fraction | None = None

    @property
    def wraps(self) -> int:
        """Number of day-boundary wraps observed so far."""
        return self._wraps

    def raw_seconds(self, stamp: Stamp) -> Fraction:
        """Seconds of a stamp within its (un-offset, un-wrapped) record day."""
        if stamp.frame >= self.frames_per_second:
            raise InputError(
                f"frame {stamp.frame} out of range for rate {self.rate} in {stamp.raw!r}",
                stage=Stage.TIMELINE,
            )
        return (
            Fraction(stamp.hour * 3600 + stamp.minute * 60 + stamp.second)
            + Fraction(stamp.frame, self.rate)
        )

    def convert(self, stamp: Stamp) -> Fraction:
        """Map a stamp to unified seconds, updating wrap state as a side effect."""
        raw = self.raw_seconds(stamp)
        # A backwards jump (strictly smaller than the previous stamp) is a day
        # wrap. Equality is a repeated stamp and must not advance the day.
        if self._last_raw is not None and raw < self._last_raw:
            self._wraps += 1
        self._last_raw = raw
        return raw + self._wraps * _SECONDS_PER_DAY + self.offset


@dataclass(frozen=True, slots=True)
class SamplePosition:
    """A timeline instant mapped onto an integer sample grid."""

    index: int
    exact: Fraction
    error_seconds: Fraction

    @property
    def abs_error_seconds(self) -> Fraction:
        return abs(self.error_seconds)


def to_sample_position(time_seconds: Fraction, sample_rate: int) -> SamplePosition:
    """Map fractional seconds to the nearest sample, retaining rounding error."""
    if sample_rate <= 0:
        raise InputError("sample rate must be positive", stage=Stage.TIMELINE)
    exact = Fraction(time_seconds) * sample_rate
    index = math.floor(exact + Fraction(1, 2))
    error = Fraction(index, sample_rate) - Fraction(time_seconds)
    return SamplePosition(index=index, exact=exact, error_seconds=error)


def master_tick_rate(sample_rates: list[int]) -> int:
    """LCM of the source sample rates: one integer grid every rate divides."""
    if not sample_rates:
        raise InputError("at least one sample rate is required", stage=Stage.TIMELINE)
    tick = 1
    for rate in sample_rates:
        if rate <= 0:
            raise InputError(f"sample rate must be positive, got {rate}", stage=Stage.TIMELINE)
        tick = math.lcm(tick, rate)
    return tick


@dataclass(frozen=True, slots=True)
class TimecodeRecord:
    line: int
    offset_bytes: int
    raw: str
    unified_seconds: Fraction | None  # None when the record was unconvertible


def load_timecode_manifest(
    path: str | Path,
) -> tuple[list[TimecodeRecord], list[ErrorRecord], TimecodeClock]:
    """Parse a manifest whose header declares ``# rate=... offset=...``.

    Unconvertible records are returned as located error-like records and left
    out of the converted list; boundaries downstream are never shifted to hide
    them. Repeated stamps are legal, and a backwards jump advances the day.
    """
    data = Path(path).read_bytes()
    header = data.split(b"\n", 1)[0].decode("utf-8", errors="replace")
    rate_text = Fraction(24)
    offset = Fraction(0)
    if header.startswith("#"):
        for token in header[1:].split():
            if token.startswith("rate="):
                rate_text = parse_rate(token.split("=", 1)[1])
            elif token.startswith("offset="):
                offset = Fraction(token.split("=", 1)[1])
    clock = TimecodeClock(rate_text, offset_seconds=offset)

    records: list[TimecodeRecord] = []
    errors: list[ErrorRecord] = []
    byte_cursor = 0
    for line_no, raw_line in enumerate(data.split(b"\n"), start=1):
        line = raw_line.decode("utf-8", errors="replace").strip()
        line_bytes = len(raw_line) + 1  # include the consumed newline
        if not line or line.startswith("#"):
            byte_cursor += line_bytes
            continue
        try:
            stamp = parse_timecode_stamp(
                line, offset_bytes=byte_cursor, line=line_no
            )
            unified = clock.convert(stamp)
            records.append(TimecodeRecord(line_no, byte_cursor, line, unified))
        except InputError as exc:
            errors.append(
                ErrorRecord(
                    stage=Stage.TIMELINE.value,
                    kind=type(exc).__name__,
                    message=exc.message,
                    source=str(path),
                    offset_bytes=byte_cursor,
                    line=line_no,
                )
            )
            records.append(TimecodeRecord(line_no, byte_cursor, line, None))
        byte_cursor += line_bytes
    return records, errors, clock
