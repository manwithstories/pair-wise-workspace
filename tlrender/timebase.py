"""Frame/rate helpers.

fps is always stored as an exact rational (Fraction). Frame numbers are
converted to whole milliseconds with half-up rounding. The conversion of
every individual value stays within 0.5 ms of the exact rational value, so
the accumulated per-cue error never exceeds 1 ms.
"""
from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction


@dataclass(frozen=True, slots=True)
class TimeBase:
    fps: Fraction

    @classmethod
    def parse(cls, text: str) -> "TimeBase":
        text = text.strip()
        if "/" in text:
            num_s, den_s = text.split("/", 1)
            num, den = int(num_s), int(den_s)
        else:
            num, den = int(text), 1
        if num <= 0 or den <= 0:
            raise ValueError(f"fps must be positive, got {text!r}")
        return cls(Fraction(num, den))

    def frame_to_ms(self, frame: int) -> int:
        """Convert an integer frame count to whole milliseconds (half-up)."""
        if frame < 0:
            raise ValueError(f"frame number must be non-negative, got {frame}")
        exact = Fraction(frame, 1) * 1000 / self.fps
        return _round_half_up(exact)

    def ms_to_frame(self, ms: int) -> int:
        if ms < 0:
            raise ValueError(f"milliseconds must be non-negative, got {ms}")
        exact = Fraction(ms, 1) * self.fps / 1000
        return _round_half_up(exact)

    def frame_span_to_ms(self, start_frame: int, end_frame: int) -> tuple[int, int]:
        start_ms = self.frame_to_ms(start_frame)
        end_ms = self.frame_to_ms(end_frame)
        if end_ms <= start_ms:
            raise ValueError(
                f"cue interval must have positive duration: frames "
                f"[{start_frame}, {end_frame})"
            )
        return start_ms, end_ms

    @property
    def fps_text(self) -> str:
        if self.fps.denominator == 1:
            return str(self.fps.numerator)
        return f"{self.fps.numerator}/{self.fps.denominator}"


def _round_half_up(value: Fraction) -> int:
    """Round a Fraction to the nearest integer, halves away from zero."""
    whole = value.numerator // value.denominator
    remainder = value.numerator % value.denominator
    doubled = remainder * 2
    if doubled > value.denominator or (
        doubled == value.denominator and value.numerator > 0
    ):
        return whole + 1
    return whole


def validate_ms_interval(start_ms: int, end_ms: int) -> None:
    if start_ms < 0 or end_ms < 0:
        raise ValueError(f"negative time in interval [{start_ms}, {end_ms})")
    if end_ms <= start_ms:
        raise ValueError(
            f"zero/negative duration interval [{start_ms}, {end_ms})"
        )
