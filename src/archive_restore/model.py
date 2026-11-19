"""Shared media timebase, packet state and offset-aware error model.

All modules (parse / repair / index / verify) use the constants and
types defined here so that timing and error reporting stay consistent.
"""

from __future__ import annotations

import enum
from dataclasses import dataclass, field

# Unified media clock: 90 kHz (RTP default video clock, used as the
# canonical archive timebase for every stream we recover).
MEDIA_CLOCK_RATE = 90_000

# RTP sequence numbers are 16 bit and wrap; timestamps are 32 bit.
SEQ_MOD = 1 << 16
SEQ_HALF = 1 << 15
TS_MOD = 1 << 32
TS_HALF = 1 << 31

# Record framing on disk: 2-byte big-endian length + one RTP/RTCP packet.
FRAME_HEADER_LEN = 2
MIN_PACKET_LEN = 12  # smallest legal RTP header

# RTCP packet types (RFC 3550).  RTP payload types 72-76 are reserved so
# that RTCP is never confused with RTP on a shared stream.
RTCP_SR = 200
RTCP_RR = 201
RTCP_SDES = 202
RTCP_BYE = 203
RTCP_APP = 204
RTCP_TYPES = {RTCP_SR, RTCP_RR, RTCP_SDES, RTCP_BYE, RTCP_APP}


class PacketStatus(enum.Enum):
    """Lifecycle state of a single packet through the pipeline."""

    OK = "ok"
    DUPLICATE = "duplicate"
    REORDERED = "reordered"
    OUT_OF_WINDOW = "out_of_window"
    CORRUPT = "corrupt"
    TRUNCATED = "truncated"
    LOST = "lost"  # inferred gap, never physically seen


class Stage(enum.Enum):
    """Pipeline stage an error or checkpoint belongs to."""

    PARSE = "parse"
    REPAIR = "repair"
    INDEX = "index"
    VERIFY = "verify"


@dataclass
class PacketError:
    """A located, explainable failure tied to raw byte offsets.

    offset_start/offset_end delimit the offending bytes in the input
    file (end exclusive).  ``stage`` says where it was detected and
    ``reason`` is a stable machine-readable cause string.
    """

    offset_start: int
    offset_end: int
    stage: Stage
    reason: str
    detail: str = ""

    def to_dict(self) -> dict:
        return {
            "offset_start": self.offset_start,
            "offset_end": self.offset_end,
            "stage": self.stage.value,
            "reason": self.reason,
            "detail": self.detail,
        }

    @classmethod
    def from_dict(cls, d: dict) -> "PacketError":
        return cls(
            offset_start=int(d["offset_start"]),
            offset_end=int(d["offset_end"]),
            stage=Stage(d["stage"]),
            reason=str(d["reason"]),
            detail=str(d.get("detail", "")),
        )


@dataclass
class RtpPacket:
    """Parsed RTP packet; payload is re-read from the input via offsets."""

    offset: int  # absolute offset of the RTP header in the input file
    length: int  # packet length in bytes (excluding frame header)
    sequence: int
    timestamp: int
    ssrc: int
    payload_type: int
    marker: bool
    payload_offset: int
    payload_length: int
    arrival_index: int = 0  # position in the physical stream


@dataclass
class SenderReport:
    """RTCP Sender Report: maps an RTP timestamp to absolute NTP time."""

    offset: int
    ssrc: int
    ntp_seconds: float  # absolute wall-clock seconds
    rtp_timestamp: int


@dataclass
class UncertainInterval:
    """A range of extended sequence numbers whose timing is unreliable.

    When timestamps cannot be inferred reliably we mark the interval
    instead of fabricating precise times.
    """

    ext_seq_start: int
    ext_seq_end: int
    reason: str


@dataclass
class RepairResult:
    """Output of the repair stage."""

    order: list[int] = field(default_factory=list)  # packet indices, emit order
    ext_seq: dict[int, int] = field(default_factory=dict)  # pkt idx -> ext seq
    ext_ts: dict[int, int] = field(default_factory=dict)  # pkt idx -> 64-bit ts
    duplicates: int = 0
    reordered: int = 0
    lost: int = 0
    out_of_window: int = 0
    uncertain: list[UncertainInterval] = field(default_factory=list)
    errors: list[PacketError] = field(default_factory=list)
