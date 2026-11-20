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
