"""RTP/RTCP parsing and per-SSRC packet organisation.

Parses raw RTP packets (RFC 3550) and RTCP Sender Reports, groups them
by SSRC, and extends 16-bit sequence to 64-bit sequence numbers and 32-bit to
64-bit RTP timestamps so wraparound is handled transparently.

The payload framing used for locally generated material is a small
self-describing header (see ``pack_payload``/``unpack_payload``) so the
recovery pipeline can reassemble encoded frames without codec-specific
depayloaders.
"""

from __future__ import annotations

import struct
from dataclasses import dataclass, field

from .errors import UnrecoverableError

RTP_VERSION = 2
RTCP_PT_SR = 200

# Payload mini-header: magic(2) frame_index(u32) part(u8) parts(u8)
# flags(u8: bit0=keyframe) codec(u8: 0=video-es, 1=audio-pcm)
PAYLOAD_MAGIC = b"AF"
PAYLOAD_HDR = struct.Struct(">2sIBBBB")
FLAG_KEYFRAME = 0x01
CODEC_VIDEO = 0
CODEC_AUDIO = 1

SEQ_MOD = 1 << 16
TS_MOD = 1 << 32


@dataclass(frozen=True)
class RtpPacket:
    seq: int  # 64-bit extended sequence number
    timestamp: int  # 64-bit extended RTP timestamp
    ssrc: int
    payload_type: int
    marker: bool
    payload: bytes
    arrival: int  # arrival order index


@dataclass(frozen=True)
class RtcpSR:
    ssrc: int
    ntp_seconds: float  # NTP timestamp as seconds
    rtp_timestamp: int  # raw 32-bit RTP timestamp


@dataclass(frozen=True)
class PayloadHeader:
    frame_index: int
    part: int
    parts: int
    keyframe: bool
    codec: int


def pack_payload(frame_index: int, part: int, parts: int, keyframe: bool,
                 codec: int, chunk: bytes) -> bytes:
    flags = FLAG_KEYFRAME if keyframe else 0
    return PAYLOAD_HDR.pack(PAYLOAD_MAGIC, frame_index, part, parts, flags,
                            codec) + chunk


def unpack_payload(payload: bytes) -> tuple[PayloadHeader, bytes]:
    if len(payload) < PAYLOAD_HDR.size:
        raise UnrecoverableError("payload shorter than mini-header")
    magic, frame_index, part, parts, flags, codec = PAYLOAD_HDR.unpack(
        payload[:PAYLOAD_HDR.size])
    if magic != PAYLOAD_MAGIC:
        raise UnrecoverableError("bad payload magic")
    if parts == 0 or part >= parts:
        raise UnrecoverableError("invalid part/parts in payload header")
    hdr = PayloadHeader(frame_index=frame_index, part=part, parts=parts,
                        keyframe=bool(flags & FLAG_KEYFRAME), codec=codec)
    return hdr, payload[PAYLOAD_HDR.size:]


def parse_rtp(data: bytes, arrival: int = 0) -> RtpPacket:
    """Parse one raw RTP packet. Raises UnrecoverableError if malformed."""
    if len(data) < 12:
        raise UnrecoverableError("RTP packet shorter than fixed header")
    b0, b1, seq, ts, ssrc = struct.unpack(">BBHII", data[:12])
    version = b0 >> 6
    if version != RTP_VERSION:
        raise UnrecoverableError(f"unsupported RTP version {version}")
    cc = b0 & 0x0F
    has_ext = bool(b0 & 0x10)
    padded = bool(b0 & 0x20)
    offset = 12 + 4 * cc
    if len(data) < offset:
        raise UnrecoverableError("RTP CSRC list overruns packet")
    if has_ext:
        if len(data) < offset + 4:
            raise UnrecoverableError("RTP extension header truncated")
        ext_len = struct.unpack(">H", data[offset + 2:offset + 4])[0]
        offset += 4 + 4 * ext_len
        if len(data) < offset:
            raise UnrecoverableError("RTP extension body truncated")
    end = len(data)
    if padded:
        pad = data[-1]
        if pad == 0 or pad > end - offset:
            raise UnrecoverableError("invalid RTP padding")
        end -= pad
    return RtpPacket(seq=seq, timestamp=ts, ssrc=ssrc, payload_type=b1 & 0x7F,
                     marker=bool(b1 & 0x80), payload=data[offset:end],
                     arrival=arrival)


def parse_rtcp_sr(data: bytes) -> RtcpSR:
    """Parse an RTCP Sender Report (packet type 200)."""
    if len(data) < 28:
        raise UnrecoverableError("RTCP SR shorter than 28 bytes")
    b0, pt, _length, ssrc = struct.unpack(">BBHI", data[:8])
    if (b0 >> 6) != RTP_VERSION or pt != RTCP_PT_SR:
        raise UnrecoverableError("not an RTCP Sender Report")
    ntp_msb, ntp_lsb, rtp_ts = struct.unpack(">III", data[8:20])
    ntp_seconds = ntp_msb + ntp_lsb / 2**32
    return RtcpSR(ssrc=ssrc, ntp_seconds=ntp_seconds, rtp_timestamp=rtp_ts)


def _extend(value: int, modulus: int, prev_ext: int | None) -> int:
    """Extend a wrapping counter to 64 bit by picking the cycle closest to
    the previously extended value (tolerates reordering within half a
    cycle in either direction)."""
    if prev_ext is None:
        return value
    base = (prev_ext // modulus) * modulus + value
    best = min((base - modulus, base, base + modulus),
               key=lambda c: (abs(c - prev_ext), c < prev_ext))
    return best


@dataclass
class _SsrcState:
    prev_seq_ext: int | None = None
    prev_ts_ext: int | None = None
    packets: list[RtpPacket] = field(default_factory=list)


class PacketOrganizer:
    """Groups parsed packets by SSRC and extends seq/timestamp to 64 bit."""

    def __init__(self) -> None:
        self._streams: dict[int, _SsrcState] = {}

    def push(self, pkt: RtpPacket) -> RtpPacket:
        st = self._streams.setdefault(pkt.ssrc, _SsrcState())
        seq_ext = _extend(pkt.seq, SEQ_MOD, st.prev_seq_ext)
        ts_ext = _extend(pkt.timestamp, TS_MOD, st.prev_ts_ext)
        st.prev_seq_ext = max(st.prev_seq_ext or 0, seq_ext)
        st.prev_ts_ext = max(st.prev_ts_ext or 0, ts_ext)
        ext = RtpPacket(seq=seq_ext, timestamp=ts_ext, ssrc=pkt.ssrc,
                        payload_type=pkt.payload_type, marker=pkt.marker,
                        payload=pkt.payload, arrival=pkt.arrival)
        st.packets.append(ext)
        return ext

    def streams(self) -> dict[int, list[RtpPacket]]:
        return {ssrc: st.packets for ssrc, st in self._streams.items()}
