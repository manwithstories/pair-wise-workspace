"""Streaming boundary scanners for H.264/H.265 Annex B and AV1 OBU streams.

Scans run in fixed-size chunks so a whole stream is never held in memory.
All reported offsets are relative to the scanned file; the pipeline maps
them back to original input byte offsets.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import BinaryIO

from .errors import ErrorRecord, Recovery, Stage

CHUNK_SIZE = 1 << 16
START_CODE = b"\x00\x00\x01"

H264_IDR = 5
H265_KEYFRAME_TYPES = {19, 20}  # IDR_W_RADL, IDR_N_LP

OBU_SEQUENCE_HEADER = 1
OBU_TEMPORAL_DELIMITER = 2
OBU_FRAME_HEADER = 3
OBU_FRAME = 6

ANNEXB_CODECS = ("h264", "h265")
CODECS = ANNEXB_CODECS + ("av1",)


@dataclass
class Unit:
    """One elementary stream unit (NAL or OBU)."""

    codec: str
    unit_type: int
    offset: int  # offset of the unit (incl. start code / OBU header)
    length: int
    is_keyframe: bool
    complete: bool = True


@dataclass
class ScanResult:
    units: list[Unit] = field(default_factory=list)
    errors: list[ErrorRecord] = field(default_factory=list)

    @property
    def keyframes(self) -> list[Unit]:
        return [u for u in self.units if u.is_keyframe and u.complete]


def _classify_annexb(codec: str, hdr: bytes) -> tuple[int, bool]:
    if codec == "h264":
        nal_type = hdr[0] & 0x1F
        return nal_type, nal_type == H264_IDR
    nal_type = (hdr[0] >> 1) & 0x3F
    return nal_type, nal_type in H265_KEYFRAME_TYPES


def scan_annexb(path: str, codec: str, tail_complete: bool = True) -> ScanResult:
    """Scan an Annex B stream, classifying truncation and length defects."""
    result = ScanResult()
    buf = b""
    base = 0  # file offset of buf[0]
    pending_off: int | None = None  # start offset of current unit
    pending_hdr = b""
    saw_garbage = False

    def close_unit(end_off: int) -> None:
        nonlocal pending_off, pending_hdr
        if pending_off is None:
            return
        length = end_off - pending_off
        prefix = 4 if length >= 4 else 3
        payload_len = length - prefix
        if payload_len <= 0:
            result.errors.append(ErrorRecord(
                stage=Stage.PARSE.value, track="",
                message=f"invalid length: empty {codec} unit",
                byte_offset=pending_off, recovery=Recovery.DROPPED_UNIT.value))
        else:
            nal_type, is_key = _classify_annexb(codec, pending_hdr)
            result.units.append(Unit(codec, nal_type, pending_off, length, is_key))
        pending_off = None
        pending_hdr = b""

    with open(path, "rb") as f:
        while True:
            chunk = f.read(CHUNK_SIZE)
            eof = not chunk
            buf += chunk
            limit = len(buf) if eof else max(0, len(buf) - 8)
            i = 0
            while True:
                j = buf.find(START_CODE, i, limit if not eof else len(buf))
                if j < 0 or j + 3 > limit:
                    break
                prefix = 4 if j > 0 and buf[j - 1] == 0 else 3
                unit_off = base + j - (1 if prefix == 4 else 0)
                if pending_off is None:
                    if unit_off > 0 and not saw_garbage:
                        saw_garbage = True
                        result.errors.append(ErrorRecord(
                            stage=Stage.PARSE.value, track="",
                            message="truncated head: bytes before first start code",
                            byte_offset=0, recovery=Recovery.SKIPPED_GARBAGE.value))
                else:
                    close_unit(base + j - (1 if prefix == 4 else 0))
                pending_off = unit_off
                hdr_start = j + 3
                pending_hdr = bytes(buf[hdr_start:hdr_start + 2])
                i = hdr_start
            if eof:
                break
            consumed = max(0, min(limit, len(buf) - 8))
            base += consumed
            buf = buf[consumed:]
    if pending_off is not None:
        end_off = base + len(buf)
        length = end_off - pending_off
        if length > 4 and pending_hdr:
            nal_type, is_key = _classify_annexb(codec, pending_hdr)
            complete = tail_complete
            result.units.append(Unit(codec, nal_type, pending_off, length, is_key, complete))
            if not complete:
                result.errors.append(ErrorRecord(
                    stage=Stage.PARSE.value, track="",
                    message="incomplete tail: final unit cut off at end of stream",
                    byte_offset=pending_off, recovery=Recovery.DROPPED_UNIT.value))
        elif length > 0:
            result.errors.append(ErrorRecord(
                stage=Stage.PARSE.value, track="",
                message="incomplete tail: truncated unit header",
                byte_offset=pending_off, recovery=Recovery.DROPPED_UNIT.value))
    return result


def _read_leb128(f: BinaryIO) -> tuple[int, int]:
    """Read a LEB128 value; returns (value, byte_count). Raises on malformed."""
    value = 0
    for i in range(8):
        b = f.read(1)
        if not b:
            raise EOFError("truncated leb128")
        value |= (b[0] & 0x7F) << (7 * i)
        if not b[0] & 0x80:
            return value, i + 1
    raise ValueError("malformed leb128: more than 8 bytes")


def scan_av1(path: str, tail_complete: bool = True) -> ScanResult:
    """Scan an AV1 low-overhead OBU stream unit by unit."""
    result = ScanResult()
    with open(path, "rb") as f:
        offset = 0
        while True:
            hdr = f.read(1)
            if not hdr:
                break
            b0 = hdr[0]
            if b0 & 0x80:
                result.errors.append(ErrorRecord(
                    stage=Stage.PARSE.value, track="",
                    message="invalid OBU: forbidden bit set",
                    byte_offset=offset, recovery=Recovery.DROPPED_UNIT.value))
                break
            obu_type = (b0 >> 3) & 0x0F
            has_ext = (b0 >> 2) & 1
            has_size = (b0 >> 1) & 1
            header_len = 1 + (1 if has_ext else 0)
            if has_ext:
                f.read(1)
            try:
                if has_size:
                    size, n = _read_leb128(f)
                    header_len += n
                else:  # no size field: unit runs to end of file
                    f.seek(0, 2)
                    size = f.tell() - (offset + header_len)
            except (EOFError, ValueError) as exc:
                result.errors.append(ErrorRecord(
                    stage=Stage.PARSE.value, track="",
                    message=f"invalid length: {exc}",
                    byte_offset=offset, recovery=Recovery.DROPPED_UNIT.value))
                break
            payload_off = offset + header_len
            first = f.read(1)
            is_key = False
            if obu_type in (OBU_FRAME_HEADER, OBU_FRAME) and first:
                show_existing = first[0] & 0x80
                if not show_existing:
                    frame_type = (first[0] >> 5) & 0x03
                    is_key = frame_type == 0  # KEY_FRAME
            remaining = size - 1 if first else size
            f.seek(max(0, remaining), 1)
            end = f.tell()
            complete = remaining <= 0 or end == payload_off + size
            if not has_size:
                complete = tail_complete
            result.units.append(Unit("av1", obu_type, offset, header_len + max(0, size), is_key, complete))
            if not complete:
                result.errors.append(ErrorRecord(
                    stage=Stage.PARSE.value, track="",
                    message="incomplete tail: OBU payload shorter than declared size",
                    byte_offset=offset, recovery=Recovery.DROPPED_UNIT.value))
                break
            offset = end
    return result


def scan(path: str, codec: str, tail_complete: bool = True) -> ScanResult:
    if codec in ANNEXB_CODECS:
        return scan_annexb(path, codec, tail_complete)
    if codec == "av1":
        return scan_av1(path, tail_complete)
    raise ValueError(f"unsupported codec: {codec}")
