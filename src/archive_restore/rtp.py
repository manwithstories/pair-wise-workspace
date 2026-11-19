"""RTP/RTCP parsing with byte-offset-accurate error reporting.

On-disk stream format: repeated records of
    [2-byte big-endian length][one RTP or RTCP packet]
A single bad record never discards later parseable data: framing errors
trigger a bounded resynchronisation scan for the next plausible record.
"""

from __future__ import annotations

import struct
from dataclasses import dataclass, field

from .model import (
    FRAME_HEADER_LEN,
    MIN_PACKET_LEN,
    RTCP_SR,
    RTCP_TYPES,
    PacketError,
    RtpPacket,
    SenderReport,
    Stage,
)

MAX_PACKET_LEN = 65535


class PacketParseError(Exception):
    """Raised when a single record's payload is not a valid packet."""

    def __init__(self, reason: str, detail: str = ""):
        super().__init__(reason)
        self.reason = reason
        self.detail = detail


@dataclass
class ParseResult:
    packets: list[RtpPacket] = field(default_factory=list)
    sender_reports: list[SenderReport] = field(default_factory=list)
    errors: list[PacketError] = field(default_factory=list)
    records_seen: int = 0


def parse_rtp_packet(buf: bytes, offset: int, arrival_index: int) -> RtpPacket:
    """Parse one RTP packet from ``buf`` located at absolute ``offset``."""
    if len(buf) < MIN_PACKET_LEN:
        raise PacketParseError("short_header", f"rtp header needs 12 bytes, got {len(buf)}")
    b0, b1, seq, ts, ssrc = struct.unpack(">BBHII", buf[:12])
    version = b0 >> 6
    if version != 2:
        raise PacketParseError("bad_version", f"rtp version {version}, expected 2")
    padding = (b0 >> 5) & 1
    extension = (b0 >> 4) & 1
    cc = b0 & 0x0F
    marker = bool(b1 & 0x80)
    payload_type = b1 & 0x7F

    header_len = 12 + 4 * cc
    if len(buf) < header_len:
        raise PacketParseError(
            "short_csrc", f"cc={cc} needs {header_len} bytes, got {len(buf)}"
        )
    if extension:
        if len(buf) < header_len + 4:
            raise PacketParseError("short_extension", "extension header truncated")
        _profile, ext_words = struct.unpack(">HH", buf[header_len : header_len + 4])
        ext_len = 4 + ext_words * 4
        if len(buf) < header_len + ext_len:
            raise PacketParseError(
                "short_extension_data",
                f"extension needs {ext_len} bytes, got {len(buf) - header_len}",
            )
        header_len += ext_len

    payload_end = len(buf)
    if padding:
        if payload_end == header_len:
            raise PacketParseError("bad_padding", "padding bit set with no payload")
        pad_len = buf[-1]
        if pad_len == 0 or pad_len > payload_end - header_len:
            raise PacketParseError("bad_padding", f"invalid padding count {pad_len}")
        payload_end -= pad_len

    return RtpPacket(
        offset=offset,
        length=len(buf),
        sequence=seq,
        timestamp=ts,
        ssrc=ssrc,
        payload_type=payload_type,
        marker=marker,
        payload_offset=offset + header_len,
        payload_length=payload_end - header_len,
        arrival_index=arrival_index,
    )


def parse_rtcp_compound(buf: bytes, offset: int) -> list[SenderReport]:
    """Parse a compound RTCP packet; return any Sender Reports found."""
    srs: list[SenderReport] = []
    pos = 0
    while pos < len(buf):
        if len(buf) - pos < 4:
            raise PacketParseError("short_rtcp_header", "rtcp header truncated")
        b0, ptype, length_words = struct.unpack(">BBH", buf[pos : pos + 4])
        if b0 >> 6 != 2:
            raise PacketParseError("bad_version", f"rtcp version {b0 >> 6}, expected 2")
        pkt_len = (length_words + 1) * 4
        if pkt_len < 4 or pos + pkt_len > len(buf):
            raise PacketParseError(
                "bad_rtcp_length", f"rtcp length {pkt_len} exceeds record"
            )
        if ptype == RTCP_SR:
            if pkt_len < 28:
                raise PacketParseError("short_sr", "sender report truncated")
            ssrc, ntp_sec, ntp_frac, rtp_ts = struct.unpack(
                ">IIII", buf[pos + 4 : pos + 20]
            )
            srs.append(
                SenderReport(
                    offset=offset + pos,
                    ssrc=ssrc,
                    ntp_seconds=ntp_sec + ntp_frac / (1 << 32),
                    rtp_timestamp=rtp_ts,
                )
            )
        elif ptype not in RTCP_TYPES:
            raise PacketParseError("bad_rtcp_type", f"unknown rtcp type {ptype}")
        pos += pkt_len
    return srs


def _looks_like_record(data: bytes, pos: int) -> bool:
    """Heuristic: plausible frame header + version-2 packet at ``pos``."""
    if pos + FRAME_HEADER_LEN + MIN_PACKET_LEN > len(data):
        return False
    (rec_len,) = struct.unpack(">H", data[pos : pos + FRAME_HEADER_LEN])
    if rec_len < MIN_PACKET_LEN or pos + FRAME_HEADER_LEN + rec_len > len(data):
        return False
    return data[pos + FRAME_HEADER_LEN] >> 6 == 2


def parse_stream(data: bytes) -> ParseResult:
    """Parse a whole framed stream, collecting packets and located errors."""
    result = ParseResult()
    pos = 0
    n = len(data)
    while pos < n:
        if n - pos < FRAME_HEADER_LEN:
            result.errors.append(
                PacketError(pos, n, Stage.PARSE, "truncated_frame_header",
                            "fewer than 2 bytes remain for frame length")
            )
            break
        (rec_len,) = struct.unpack(">H", data[pos : pos + FRAME_HEADER_LEN])
        pkt_start = pos + FRAME_HEADER_LEN
        if rec_len < MIN_PACKET_LEN or rec_len > MAX_PACKET_LEN:
            result.errors.append(
                PacketError(pos, pos + FRAME_HEADER_LEN, Stage.PARSE,
                            "bad_length_field", f"implausible length {rec_len}")
            )
            pos = _resync(data, pos + 1, result)
            continue
        if pkt_start + rec_len > n:
            if data[pkt_start] >> 6 == 2:
                result.errors.append(
                    PacketError(pos, n, Stage.PARSE, "truncated_tail",
                                f"record declares {rec_len} bytes, "
                                f"only {n - pkt_start} remain")
                )
            else:
                result.errors.append(
                    PacketError(pos, pos + FRAME_HEADER_LEN, Stage.PARSE,
                                "bad_length_field", f"length {rec_len} overruns input")
                )
                _resync(data, pos + 1, result)
            break
        record = data[pkt_start : pkt_start + rec_len]
        result.records_seen += 1
        ptype = record[1] & 0x7F if rec_len >= 2 else 0
        try:
            if ptype in RTCP_TYPES:
                result.sender_reports.extend(parse_rtcp_compound(record, pkt_start))
            else:
                pkt = parse_rtp_packet(record, pkt_start, result.records_seen - 1)
                result.packets.append(pkt)
        except PacketParseError as exc:
            result.errors.append(
                PacketError(pkt_start, pkt_start + rec_len, Stage.PARSE,
                            exc.reason, exc.detail)
            )
        pos = pkt_start + rec_len
    return result


def _resync(data: bytes, start: int, result: ParseResult) -> int:
    """Scan forward for the next plausible record boundary."""
    n = len(data)
    pos = start
    while pos < n:
        if _looks_like_record(data, pos):
            return pos
        pos += 1
    return n
