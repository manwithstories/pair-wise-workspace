"""Small immutable records passed between pipeline stages."""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Optional


def seconds_to_ms(value: Fraction | float | None) -> float | None:
    if value is None:
        return None
    return round(float(value) * 1000.0, 3)


def ticks_to_ms(ticks: Optional[int], time_base: Fraction) -> float | None:
    if ticks is None:
        return None
    return seconds_to_ms(Fraction(int(ticks)) * time_base)


@dataclass(frozen=True, slots=True)
class MediaInfo:
    path: str
    format_name: str
    stream_index: int
    codec_name: str
    time_base: Fraction
    width: int
    height: int
    average_rate: Optional[Fraction]
    duration: Optional[Fraction]


@dataclass(frozen=True, slots=True)
class PacketMeta:
    seq: int
    stream_index: int
    pts: Optional[int]
    dts: Optional[int]
    duration: Optional[int]
    time_base: Fraction
    is_keyframe: bool
    is_discard: bool
    size: int

    @property
    def pts_seconds(self) -> Optional[Fraction]:
        return None if self.pts is None else Fraction(self.pts) * self.time_base

    @property
    def dts_seconds(self) -> Optional[Fraction]:
        return None if self.dts is None else Fraction(self.dts) * self.time_base

    @property
    def pts_ms(self) -> Optional[float]:
        return seconds_to_ms(self.pts_seconds)

    @property
    def dts_ms(self) -> Optional[float]:
        return seconds_to_ms(self.dts_seconds)


@dataclass(frozen=True, slots=True)
class FrameRecord:
    index: int
    pts: Optional[int]
    dts: Optional[int]
    time_base: Fraction
    pict_type: str
    width: int
    height: int
    pixel_digest: str
    luma_mean: float
    packet_seq: Optional[int]
    packet_keyframe: Optional[bool]

    @property
    def pts_seconds(self) -> Optional[Fraction]:
        return None if self.pts is None else Fraction(self.pts) * self.time_base

    @property
    def pts_ms(self) -> Optional[float]:
        return seconds_to_ms(self.pts_seconds)

    @property
    def dts_ms(self) -> Optional[float]:
        return None if self.dts is None else seconds_to_ms(Fraction(self.dts) * self.time_base)


@dataclass(frozen=True, slots=True)
class RepairEntry:
    packet_seq: Optional[int]
    frame_index: Optional[int]
    field: str
    reason: str
    before_ms: Optional[float]
    after_ms: Optional[float]
    delta_ms: Optional[float] = None

    def as_dict(self) -> dict[str, object]:
        return {
            "packet_seq": self.packet_seq,
            "frame_index": self.frame_index,
            "field": self.field,
            "reason": self.reason,
            "before_ms": self.before_ms,
            "after_ms": self.after_ms,
            "delta_ms": self.delta_ms,
        }


def frame_type_name(pict_type: object) -> str:
    """Normalize PyAV picture-type enums across PyAV 12 and 18."""
    try:
        return {1: "I", 2: "P", 3: "B"}.get(int(pict_type), "?")
    except (TypeError, ValueError):
        pass
    value = getattr(pict_type, "name", str(pict_type)).upper()
    if value in {"I", "P", "B"}:
        return value
    for name in ("I", "P", "B"):
        if name in value:
            return name
    return "?"
