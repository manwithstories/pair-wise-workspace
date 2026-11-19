"""Shared model: frames, tracks, integer-nanosecond time base, unified errors.

All modules exchange the dataclasses defined here.  Time is always carried
as integer nanoseconds on a common 64-bit timeline; device clocks may wrap
at ``TIMESTAMP_WRAP_NS`` and are unwrapped by the demuxer onto this base.
Errors carry the raw byte offset of the offending input record so a bad
record can always be located in the source file.  Frame payloads are never
placed inside error messages or logs.
"""
from __future__ import annotations

import enum
from dataclasses import dataclass, field
from typing import Any

NS_PER_SECOND = 1_000_000_000
INT64_MAX = (1 << 63) - 1

# Device clocks tick in nanoseconds modulo this value (~18.3 minutes),
# modelling the timestamp wraparound seen when cameras roll over.
TIMESTAMP_WRAP_NS = 1 << 40

# Safety limits (constraint: bound single-frame size and metadata size).
MAX_FRAME_BYTES = 64 * 1024 * 1024
MAX_META_BYTES = 4096
MAX_DIMENSION = 8192
MAX_QUEUE_BYTES = 256 * 1024 * 1024

# Pixel formats we can losslessly remux into the delivery container.
PIX_FMTS = {0: "gray", 1: "rgb24", 2: "yuv420p", 3: "rgba"}
PIX_FMT_IDS = {name: code for code, name in PIX_FMTS.items()}
# Parseable but not supported by the delivery container: locatable error,
# never an implicit re-encode.
UNSUPPORTED_PIX_FMTS = {4: "bayer_rg16"}
ALL_PIX_FMTS = {**PIX_FMTS, **UNSUPPORTED_PIX_FMTS}


def frame_nbytes(pixfmt: str, width: int, height: int) -> int:
    """Exact payload size for a packed frame, or -1 if layout unknown."""
    if pixfmt == "gray":
        return width * height
    if pixfmt == "rgb24":
        return width * height * 3
    if pixfmt == "rgba":
        return width * height * 4
    if pixfmt == "yuv420p":
        return width * height * 3 // 2
    if pixfmt == "bayer_rg16":
        return width * height * 2
    return -1


class Stage(enum.Enum):
    """Pipeline stages; failures are reported with the stage name."""

    PARSE = "parse"
    TRANSFORM = "transform"
    MUX = "mux"
    VALIDATE = "validate"


@dataclass
class FieldPackError(Exception):
    """Unified error object.  ``offset`` is the raw byte offset of the
    offending input record (None when not tied to a record)."""

    code: str
    message: str
    stage: Stage = Stage.PARSE
    offset: int | None = None
    recoverable: bool = True

    def __str__(self) -> str:  # never includes payload data
        loc = f" @offset={self.offset}" if self.offset is not None else ""
        return f"[{self.stage.value}:{self.code}]{loc} {self.message}"

    def as_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "stage": self.stage.value,
            "offset": self.offset,
            "message": self.message,
            "recoverable": self.recoverable,
        }


@dataclass
class FrameRecord:
    """One parsed frame record straight from the source file."""

    offset: int
    version: int
    device_id: int
    seq: int
    raw_ts_ns: int  # device clock, may have wrapped
    pixfmt: str
    width: int
    height: int
    keyframe: bool
    meta: bytes  # opaque metadata blob (bounded by MAX_META_BYTES)
    payload: bytes
    exposure_ns: int | None = None  # v2 records only
    iso: int | None = None


@dataclass
class Segment:
    """A contiguous run of frames from one device (one storage card)."""

    device_id: int
    index: int
    start_offset: int


@dataclass
class TrackInfo:
    """Probed static properties of one device track."""

    device_id: int
    pixfmt: str
    width: int
    height: int
    record_count: int = 0


# ---- demux events -------------------------------------------------------


@dataclass
class SegmentStart:
    segment: Segment


@dataclass
class FrameReady:
    """A frame placed on the unified nanosecond timeline."""

    record: FrameRecord
    pts_ns: int  # unwrapped, monotonic within its segment
    segment: Segment


@dataclass
class Quarantined:
    """A record that cannot be disambiguated; isolated, never reordered."""

    record: FrameRecord
    reason: str


DemuxEvent = SegmentStart | FrameReady | Quarantined
