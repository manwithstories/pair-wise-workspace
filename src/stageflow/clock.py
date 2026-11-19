"""Media time base: unify samples, frames and integer ticks.

One master integer tick rate (MASTER_RATE) is the single time base shared by
the audio timeline and the video frame timestamps.  All conversions are exact
integer/rational arithmetic; when a conversion must round, the rounding error
is returned alongside so callers can record it (determinism requirement).

  sample position  --(rate)-->  master ticks  --(fps)-->  frame index
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction

# Master tick rate: divisible by 24/25/30/60 fps and common sample rates
# (44100 = 2^2*3^2*5^2*7^2, 48000 = 2^7*3*5^3).  LCM-friendly high rate.
MASTER_RATE = 44_100 * 160  # 7_056_000 ticks per second; exact for 44.1k/48k/32k and 24/25/30/60fps


@dataclass(frozen=True)
class RoundingRecord:
    """Recorded rounding error for one conversion."""

    stage: str
    track: str | None
    direction: str  # e.g. "samples->ticks"
    error_ticks: Fraction  # signed error in master ticks


def samples_to_ticks(samples: int, rate: int) -> int:
    """Exact: sample index -> master ticks. Raises if not exact."""
    num = samples * MASTER_RATE
    if num % rate:
        raise ValueError(f"inexact sample->tick conversion: {samples}@{rate}")
    return num // rate


def ticks_to_samples(ticks: int, rate: int) -> tuple[int, Fraction]:
    """Master ticks -> nearest sample index; returns (samples, error_ticks)."""
    num = ticks * rate
    q, r = divmod(num, MASTER_RATE)
    # round-half-even on the remainder for determinism
    doubled = 2 * r
    if doubled > MASTER_RATE or (doubled == MASTER_RATE and q % 2 == 1):
        q += 1
        r -= MASTER_RATE
    error = Fraction(-r, rate)  # ticks of error introduced, in master-tick units
    return q, error


def ticks_to_frame(ticks: int, fps: int) -> tuple[int, Fraction]:
    """Master ticks -> nearest frame index; returns (frame, error_ticks)."""
    num = ticks * fps
    q, r = divmod(num, MASTER_RATE)
    doubled = 2 * r
    if doubled > MASTER_RATE or (doubled == MASTER_RATE and q % 2 == 1):
        q += 1
        r -= MASTER_RATE
    return q, Fraction(-r, fps)


def frame_to_ticks(frame: int, fps: int) -> int:
    """Exact: frame index -> master ticks (frame start)."""
    num = frame * MASTER_RATE
    if num % fps:
        raise ValueError(f"inexact frame->tick conversion: frame {frame} @{fps}fps")
    return num // fps


def ticks_to_ms(ticks: int) -> float:
    return ticks * 1000.0 / MASTER_RATE


def seconds_to_ticks(seconds: float) -> int:
    return round(seconds * MASTER_RATE)
