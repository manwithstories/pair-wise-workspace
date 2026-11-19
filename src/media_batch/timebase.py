"""Unified time base: rational frame rates, integer audio samples, monotonic conversion.

All arithmetic uses integer sample counts and Fraction frame rates; no float
time is ever accumulated.
"""

from __future__ import annotations

from fractions import Fraction

FPS_24 = Fraction(24, 1)
FPS_25 = Fraction(25, 1)
FPS_30 = Fraction(30, 1)
FPS_2997 = Fraction(30000, 1001)  # NTSC "29.97"

NAMED_RATES: dict[str, Fraction] = {
    "24": FPS_24,
    "25": FPS_25,
    "30": FPS_30,
    "29.97": FPS_2997,
}

DEFAULT_SAMPLE_RATE = 48000


def parse_frame_rate(name: str) -> Fraction:
    if name in NAMED_RATES:
        return NAMED_RATES[name]
    if "/" in name:
        num, den = name.split("/", 1)
        return Fraction(int(num), int(den))
    return Fraction(name)


def ms_to_samples(ms: int, sample_rate: int) -> int:
    """Exact rational conversion, rounded to nearest sample."""
    return (ms * sample_rate * 2 + 1000) // 2000


def samples_to_ms(samples: int, sample_rate: int) -> int:
    return (samples * 2000 + sample_rate) // (2 * sample_rate)


def frames_to_samples(frames: int, fps: Fraction, sample_rate: int) -> int:
    """Frame index -> audio sample index (nearest sample, exact rational math)."""
    scaled = Fraction(frames, 1) * sample_rate / fps
    return int(scaled + Fraction(1, 2)) if scaled >= 0 else 0


def samples_to_frames(samples: int, sample_rate: int, fps: Fraction) -> int:
    """Audio sample index -> frame index (floor, exact rational math)."""
    return int(Fraction(samples, 1) * fps / sample_rate)


def frame_duration_samples(fps: Fraction, sample_rate: int) -> Fraction:
    return Fraction(sample_rate, 1) / fps


class MonotonicClock:
    """Tracks a strictly non-decreasing integer sample position.

    Regressions and duplicates are reported via the error log, never silently
    reordered.
    """

    def __init__(self, error_log=None, source: str = ""):
        self._last: int | None = None
        self._log = error_log
        self._source = source

    def observe(self, sample_pos: int, record_index: int = -1,
                source_offset: int = -1) -> bool:
        """Record a position. Returns True if monotonic (>= last)."""
        if self._last is not None and sample_pos < self._last:
            if self._log is not None:
                self._log.add(
                    "timestamp_regression",
                    f"sample position {sample_pos} regresses below {self._last}",
                    self._source, source_offset, record_index)
            return False
        if self._last is not None and sample_pos == self._last and self._log is not None:
            self._log.add(
                "timestamp_duplicate",
                f"duplicate sample position {sample_pos}",
                self._source, source_offset, record_index, severity="warning")
        self._last = sample_pos
        return True

    @property
    def last(self) -> int | None:
        return self._last
