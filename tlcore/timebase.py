"""Rational time conversion with a canonical 1/48000 second grid."""

from __future__ import annotations

from fractions import Fraction
from typing import Any

SAMPLE_RATE = 48_000
DEFAULT_FPS = Fraction(30_000, 1_001)


class TimebaseError(ValueError):
    """Raised when a time value cannot be represented safely."""


def as_fraction(value: Any) -> Fraction:
    if isinstance(value, Fraction):
        return value
    if isinstance(value, int):
        return Fraction(value)
    if isinstance(value, float):
        if value != value or value in (float("inf"), float("-inf")):
            raise TimebaseError(f"non-finite time value: {value!r}")
        return Fraction(str(value))
    if isinstance(value, str):
        text = value.strip()
        if not text:
            raise TimebaseError("empty time value")
        if ":" in text:
            return _clock_fraction(text)
        try:
            return Fraction(text)
        except (ValueError, ZeroDivisionError) as exc:
            raise TimebaseError(f"invalid rational time: {value!r}") from exc
    raise TimebaseError(f"unsupported time value: {value!r}")


def _clock_fraction(text: str) -> Fraction:
    fields = text.split(":")
    if len(fields) not in (2, 3):
        raise TimebaseError(f"invalid clock time: {text!r}")
    try:
        numbers = [Fraction(part) for part in fields]
    except (ValueError, ZeroDivisionError) as exc:
        raise TimebaseError(f"invalid clock time: {text!r}") from exc
    seconds = numbers[-1]
    seconds += numbers[-2] * 60
    if len(numbers) == 3:
        seconds += numbers[0] * 3600
    return seconds


def parse_rate(value: Any) -> Fraction:
    rate = as_fraction(value)
    if rate <= 0:
        raise TimebaseError(f"frame rate must be positive: {value!r}")
    return rate


def round_half_even(value: Fraction) -> int:
    return int(round(value))


def seconds_to_samples(value: Any) -> int:
    return round_half_even(as_fraction(value) * SAMPLE_RATE)


def ms_to_samples(value: Any) -> int:
    return round_half_even(as_fraction(value) * SAMPLE_RATE / 1000)


def frames_to_samples(frames: Any, fps: Any = DEFAULT_FPS) -> int:
    return round_half_even(as_fraction(frames) * SAMPLE_RATE / parse_rate(fps))


def samples_to_seconds(samples: int) -> Fraction:
    return Fraction(samples, SAMPLE_RATE)


def samples_to_ms(samples: int) -> Fraction:
    return Fraction(samples * 1000, SAMPLE_RATE)


def samples_to_frames(samples: int, fps: Any = DEFAULT_FPS) -> Fraction:
    return Fraction(samples * parse_rate(fps), SAMPLE_RATE)


def parse_samples(value: Any, fps: Any = DEFAULT_FPS) -> int:
    """Parse an exchange time value into canonical sample units.

    Numbers and plain strings are seconds. A mapping may contain exactly one
    of ``samples``, ``seconds``, ``ms`` or ``frames``. ``value``/``rate`` is a
    rational seconds value. A string suffix of ``ms``, ``s`` or ``f`` is
    accepted for compact fixture authoring.
    """
    if isinstance(value, dict):
        keys = [key for key in ("samples", "seconds", "ms", "frames", "value") if key in value]
        if len(keys) != 1:
            raise TimebaseError(f"time mapping needs one time key: {value!r}")
        key = keys[0]
        if key == "samples":
            return _samples_int(value[key])
        if key == "seconds":
            return seconds_to_samples(value[key])
        if key == "ms":
            return ms_to_samples(value[key])
        if key == "frames":
            return frames_to_samples(value[key], value.get("fps", fps))
        rate = parse_rate(value.get("rate", 1))
        return seconds_to_samples(Fraction(as_fraction(value[key])) / rate)
    if isinstance(value, str):
        text = value.strip()
        lower = text.lower()
        try:
            if lower.endswith("ms"):
                return ms_to_samples(text[:-2])
            if lower.endswith("s"):
                return seconds_to_samples(text[:-1])
            if lower.endswith("f"):
                return frames_to_samples(text[:-1], fps)
        except (ValueError, ZeroDivisionError) as exc:
            raise TimebaseError(f"invalid time value: {value!r}") from exc
    return seconds_to_samples(value)


def _samples_int(value: Any) -> int:
    if isinstance(value, bool):
        raise TimebaseError("boolean is not a sample count")
    number = as_fraction(value)
    if number.denominator != 1:
        raise TimebaseError(f"sample count must be integral: {value!r}")
    return int(number)


def format_seconds(samples: int, digits: int = 9) -> str:
    text = f"{float(samples_to_seconds(samples)):.{digits}f}".rstrip("0").rstrip(".")
    return text or "0"


def format_clock(samples: int, digits: int = 9) -> str:
    sign = "-" if samples < 0 else ""
    remaining = abs(samples)
    hours, remaining = divmod(remaining, SAMPLE_RATE * 3600)
    minutes, remaining = divmod(remaining, SAMPLE_RATE * 60)
    seconds = float(Fraction(remaining, SAMPLE_RATE))
    return f"{sign}{hours:02d}:{minutes:02d}:{seconds:0{3 + digits}.{digits}f}"


def time_to_json(samples: int) -> dict[str, int]:
    return {"samples": int(samples)}


def is_ms_aligned(samples: int) -> bool:
    return (samples * 1000) % SAMPLE_RATE == 0


def samples_to_bankers_ms(samples: int) -> int:
    return round_half_even(samples_to_ms(samples))
