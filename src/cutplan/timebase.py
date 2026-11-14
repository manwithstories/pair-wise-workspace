from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal, ROUND_HALF_UP
from fractions import Fraction
import math
import re

from .errors import CutplanError


SUPPORTED_RATES: tuple[str, ...] = ("24", "25", "30", "30000/1001")
_RATE_ALIASES = {
    "24": (24, 1),
    "25": (25, 1),
    "30": (30, 1),
    "30000/1001": (30000, 1001),
    "29.97": (30000, 1001),
}
_VALID = {Fraction(n, d) for n, d in _RATE_ALIASES.values()}


@dataclass(frozen=True, slots=True)
class FrameRate:
    numerator: int
    denominator: int = 1

    def __post_init__(self) -> None:
        if not isinstance(self.numerator, int) or not isinstance(self.denominator, int):
            raise CutplanError("E_RATE_INVALID", "frame rate must be rational integers")
        if self.numerator <= 0 or self.denominator <= 0:
            raise CutplanError("E_RATE_INVALID", "frame rate must be positive")
        if Fraction(self.numerator, self.denominator) not in _VALID:
            raise CutplanError(
                "E_RATE_INVALID",
                f"unsupported frame rate; supported: {', '.join(SUPPORTED_RATES)}",
            )

    @property
    def fps(self) -> Fraction:
        return Fraction(self.numerator, self.denominator)

    @property
    def is_drop_frame(self) -> bool:
        return self.fps == Fraction(30000, 1001)

    @classmethod
    def parse(cls, value: object) -> "FrameRate":
        return parse_rate(value)

    def __str__(self) -> str:
        if self.denominator == 1:
            return str(self.numerator)
        return f"{self.numerator}/{self.denominator}"


def parse_rate(value: object) -> FrameRate:
    if isinstance(value, FrameRate):
        return value
    if isinstance(value, bool):
        raise CutplanError("E_RATE_INVALID", "boolean is not a frame rate")
    if isinstance(value, int):
        text = str(value)
    elif isinstance(value, float):
        if not math.isfinite(value):
            raise CutplanError("E_RATE_INVALID", "frame rate must be finite")
        text = str(int(value)) if value.is_integer() else repr(value)
    elif isinstance(value, Fraction):
        text = f"{value.numerator}/{value.denominator}"
    else:
        text = str(value).strip()
    pair = _RATE_ALIASES.get(text)
    if pair is None:
        raise CutplanError(
            "E_RATE_INVALID",
            f"unsupported frame rate {text!r}; supported: {', '.join(SUPPORTED_RATES)}",
        )
    return FrameRate(*pair)


@dataclass(frozen=True, slots=True)
class RationalTime:
    frames: int
    rate: FrameRate

    def __post_init__(self) -> None:
        if not isinstance(self.frames, int):
            raise CutplanError("E_TIME_INVALID", "rational time frames must be an integer")

    @property
    def value(self) -> int:
        return self.frames

    @property
    def seconds(self) -> Fraction:
        return Fraction(self.frames, 1) / self.rate.fps

    def rescale(self, rate: FrameRate) -> "RationalTime":
        scaled = Fraction(self.frames * rate.numerator, self.rate.numerator) * Fraction(
            self.rate.denominator, rate.denominator
        )
        return RationalTime(round_half_up(scaled), rate)


@dataclass(frozen=True, slots=True)
class FrameInterval:
    start: int
    end: int

    def __post_init__(self) -> None:
        if not isinstance(self.start, int) or not isinstance(self.end, int):
            raise CutplanError("E_INTERVAL_INVALID", "interval bounds must be integers")
        if self.start < 0:
            raise CutplanError("E_INTERVAL_INVALID", "interval start must be non-negative")
        if self.end < self.start:
            raise CutplanError("E_INTERVAL_INVALID", "interval end must not precede start")

    @property
    def duration(self) -> int:
        return self.end - self.start

    @property
    def empty(self) -> bool:
        return self.start == self.end

    @property
    def half_open(self) -> tuple[int, int]:
        return (self.start, self.end)


def round_half_up(value: object) -> int:
    if isinstance(value, int):
        return value
    if isinstance(value, Fraction):
        numerator, denominator = value.numerator, value.denominator
        if numerator >= 0:
            return (2 * numerator + denominator) // (2 * denominator)
        return -((-2 * numerator + denominator) // (2 * denominator))
    decimal = value if isinstance(value, Decimal) else Decimal(str(value))
    return int(decimal.to_integral_value(rounding=ROUND_HALF_UP))


_TC_RE = re.compile(r"^(?P<h>\d+):(?P<m>\d{2}):(?P<s>\d{2})(?P<sep>[:;])(?P<f>\d{2})$")


def parse_timecode(text: str, rate: object) -> int:
    frame_rate = parse_rate(rate)
    match = _TC_RE.fullmatch(str(text).strip())
    if not match:
        raise CutplanError("E_TIMECODE_INVALID", f"invalid timecode {text!r}")
    hours = int(match["h"])
    minutes = int(match["m"])
    seconds = int(match["s"])
    frames = int(match["f"])
    nominal_fps = 30 if frame_rate.is_drop_frame else int(frame_rate.fps)
    if minutes >= 60 or seconds >= 60 or frames >= nominal_fps:
        raise CutplanError("E_TIMECODE_INVALID", f"timecode field out of range: {text!r}")
    separator = match["sep"]
    if separator == ";" and not frame_rate.is_drop_frame:
        raise CutplanError("E_TIMECODE_INVALID", "drop-frame timecode requires 30000/1001")
    if separator == ";" and seconds == 0 and frames < 2 and minutes % 10 != 0:
        raise CutplanError("E_TIMECODE_INVALID", f"invalid drop-frame label: {text!r}")
    nominal = ((hours * 60 + minutes) * 60 + seconds) * nominal_fps + frames
    if separator == ";":
        total_minutes = hours * 60 + minutes
        nominal -= 2 * (total_minutes - total_minutes // 10)
    return nominal


def frames_to_timecode(frames: int, rate: object, drop_frame: bool | None = None) -> str:
    frame_rate = parse_rate(rate)
    if not isinstance(frames, int) or frames < 0:
        raise CutplanError("E_TIMECODE_INVALID", "frame number must be a non-negative integer")
    use_drop = frame_rate.is_drop_frame if drop_frame is None else bool(drop_frame)
    if use_drop and not frame_rate.is_drop_frame:
        raise CutplanError("E_TIMECODE_INVALID", "drop-frame timecode requires 30000/1001")
    value = frames
    separator = ";" if use_drop else ":"
    if use_drop:
        per_10m, per_min, drop = 17982, 1798, 2
        tens, remainder = divmod(value, per_10m)
        value += drop * 9 * tens
        if remainder > drop:
            value += drop * ((remainder - drop) // per_min)
    nominal_fps = 30 if use_drop else int(frame_rate.fps)
    hours, rem = divmod(value, nominal_fps * 3600)
    minutes, rem = divmod(rem, nominal_fps * 60)
    seconds, frame = divmod(rem, nominal_fps)
    return f"{hours:02d}:{minutes:02d}:{seconds:02d}{separator}{frame:02d}"


def frames_to_seconds(frames: int, rate: object) -> Fraction:
    return Fraction(frames, 1) / parse_rate(rate).fps


def seconds_to_frames(seconds: object, rate: object) -> int:
    value = seconds if isinstance(seconds, Fraction) else Fraction(str(seconds))
    return round_half_up(value * parse_rate(rate).fps)
