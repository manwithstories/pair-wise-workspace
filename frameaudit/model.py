"""Shared, lightweight records passed across the frame pipeline."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass(slots=True)
class DecodedFrame:
    decode_idx: int
    pts_raw_us: int | None
    pict_type: str
    keyframe: bool
    plane_hash: str | None
    frame: Any
    missing_pts: bool = False
    segment: int = 0
    decode_delta_us: int | None = field(default=None, init=False)
    output_pts_us: int | None = field(default=None, init=False)


@dataclass(slots=True)
class PresentedFrame:
    present_idx: int
    source: DecodedFrame
    pts_raw_us: int | None
    missing_pts: bool
    segment: int
    decode_delta_us: int | None

    @property
    def decode_idx(self) -> int:
        return self.source.decode_idx

    @property
    def pict_type(self) -> str:
        return self.source.pict_type

    @property
    def keyframe(self) -> bool:
        return self.source.keyframe

    @property
    def plane_hash(self) -> str | None:
        return self.source.plane_hash

    @property
    def frame(self) -> Any:
        return self.source.frame


@dataclass(slots=True)
class RepairedFrame:
    present_idx: int
    decode_idx: int
    pts_raw_us: int | None
    pts_fixed_us: int
    pict_type: str
    keyframe: bool
    plane_hash: str | None
    category: str | None
    frame: Any

