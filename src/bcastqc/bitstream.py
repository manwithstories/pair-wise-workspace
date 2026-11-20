"""Incremental raw-bitstream parsing and keyframe-index validation.

The parser never decodes.  It walks the byte stream, finds access-unit
boundaries and classifies keyframes purely from container syntax, so that a
batch can be validated before a single frame is decoded:

* **H.264 / H.265** - Annex B start codes (``00 00 01`` / ``00 00 00 01``),
  access units delimited by AUD (preferred) or VCL first-slice boundaries.
  IDR / IRAP NAL types mark keyframes.
* **AV1** - Low-overhead OBU stream, split on ``OBU_TEMPORAL_DELIMITER``;
  ``OBU_FRAME`` / ``OBU_FRAME_HEADER`` frame type 0 (KEY_FRAME) marks keyframes.

Everything is incremental: :class:`StreamScanner` can be fed arbitrary byte
chunks and yields access units as soon as they are complete, so memory use is
bounded by the largest access unit rather than the file size.

Index validation is separate from parsing.  A :class:`KeyframeIndex` lists
``(offset, size)`` entries.  The validator reports duplicates, out-of-order
entries and out-of-range offsets with the offending source byte position, and
either rejects the index or falls back to scanning the stream
(:func:`validate_index`).
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import Iterable, Iterator

from .errors import BitstreamError, ErrorCategory, make_error

__all__ = [
    "Codec",
    "NAL_H264",
    "NAL_HEVC",
    "AccessUnit",
    "KeyframeEntry",
    "KeyframeIndex",
    "IndexIssue",
    "IndexValidation",
    "AnnexBParser",
    "Av1ObuParser",
    "StreamScanner",
    "detect_codec",
    "validate_index",
    "compute_index",
    "file_digest_slices",
    "index_digest",
]




class Codec(str, Enum):
    H264 = "h264"
    H265 = "h265"
    AV1 = "av1"

    @classmethod
    def parse(cls, text: str) -> "Codec":
        aliases = {"h264": cls.H264, "avc": cls.H264, "h.264": cls.H264,
                   "h265": cls.H265, "hevc": cls.H265, "h.265": cls.H265,
                   "av1": cls.AV1}
        key = text.strip().lower()
        if key not in aliases:
            raise make_error(ErrorCategory.MANIFEST_INVALID, f"unknown codec {text!r}", codec=text)
        return aliases[key]


# --- H.264 NAL unit types -------------------------------------------------
NAL_H264 = {
    "SLICE_NON_IDR": 1,
    "SLICE_PART_A": 2,
    "SLICE_IDR": 5,
    "SEI": 6,
    "SPS": 7,
    "PPS": 8,
    "AUD": 9,
}
H264_KEYFRAME_NAL = frozenset({NAL_H264["SLICE_IDR"]})
H264_FIRST_SLICE_NAL = frozenset({1, 2, 5})

# --- H.265 NAL unit types (RFC 7798) --------------------------------------
NAL_HEVC = {
    "TRAIL_N": 0, "TRAIL_R": 1,
    "RASL_N": 8, "RASL_R": 9,
    "BLA_W_LP": 16, "BLA_W_RADL": 17, "BLA_N_LP": 18,
    "IDR_W_RADL": 19, "IDR_N_LP": 20,
    "CRA_NUT": 21,
    "VPS": 32, "SPS": 33, "PPS": 34,
    "AUD": 35,
}
HEVC_IRAP_NAL = frozenset({16, 17, 18, 19, 20, 21})
HEVC_VCL_MAX = 31
HEVC_PARAM_SETS = frozenset({32, 33, 34})


def _forbidden_bit(byte: int) -> bool:
    """Annex B ``forbidden_zero_bit`` is set when the top bit is 1."""
    return bool(byte & 0x80)


def _first_nal_offset(data: bytes) -> int | None:
    """Offset of the first NAL header after a leading start code, if any."""
    if data[:4] == b"\x00\x00\x00\x01":
        return 4
    if data[:3] == b"\x00\x00\x01":
        return 3
    found = _find_start_code(data, 0, len(data))
    return found[1] if found is not None else None


def _find_start_code(data: bytes, pos: int, end: int) -> tuple[int, int] | None:
    """Return ``(start_code_offset, nal_offset)`` for the next Annex B code."""
    index = data.find(b"\x00\x00\x01", pos, end)
    if index < 0:
        return None
    if index > 0 and data[index - 1] == 0:
        return index - 1, index + 3
    return index, index + 3


def detect_codec(head: bytes) -> Codec:
    """Best-effort codec detection from the first bytes of a file.

    Annex B start codes are inspected for NAL header tuples.  AV1 low-overhead
    OBU streams begin with a temporal-delimiter or sequence-header OBU whose
    top bits are ``0x0A`` (OBU_SEQUENCE_HEADER, first bit 0 = not forbidden).
    """
    if not head:
        raise make_error(ErrorCategory.STREAM_EMPTY, "empty bitstream")
    if b"\x00\x00\x01" in head[:64]:
        for offset in range(min(len(head) - 5, 64)):
            if head[offset:offset + 3] != b"\x00\x00\x01":
                continue
            nal = head[offset + 3]
            if nal & 0x80:
                continue  # H.264 forbidden_zero_bit set -> not H.264
            h264_type = nal & 0x1F
            if h264_type in (7, 8, 9) and h264_type != 0:
                return Codec.H264
            hevc_type = (nal >> 1) & 0x3F
            if hevc_type in (32, 33, 34, 35):
                return Codec.H265
        # ambiguous: prefer H.264 when a plausible SPS/PPS exists
        return Codec.H264
    first = head[0]
    if (first >> 3) & 0x0F in (1, 2, 6):
        return Codec.AV1
    raise make_error(ErrorCategory.STREAM_EMPTY, "cannot detect codec", head=head[:16].hex())


# ==========================================================================
# Access units
# ==========================================================================


@dataclass(frozen=True)
class AccessUnit:
    """One coded access unit located inside a raw stream."""

    index: int
    offset: int
    size: int
    is_keyframe: bool
    timestamp: int | None = None
    nal_types: tuple[int, ...] = ()
    error: str | None = None

    @property
    def end(self) -> int:
        return self.offset + self.size

    def to_json(self) -> dict:
        payload = {
            "index": self.index,
            "offset": self.offset,
            "size": self.size,
            "keyframe": self.is_keyframe,
        }
        if self.timestamp is not None:
            payload["timestamp"] = self.timestamp
        if self.error:
            payload["error"] = self.error
        return payload


class AnnexBParser:
    """Splits H.264/H.265 Annex B byte streams into access units."""

    def __init__(self, codec: Codec) -> None:
        if codec not in (Codec.H264, Codec.H265):
            raise ValueError("AnnexBParser handles H.264/H.265 only")
        self.codec = codec
        self._aud = NAL_H264["AUD"] if codec is Codec.H264 else NAL_HEVC["AUD"]
        self._first_slice = H264_FIRST_SLICE_NAL if codec is Codec.H264 else frozenset(range(0, 10)) | frozenset(range(16, 22))
        self._prefix = frozenset({NAL_H264["SEI"], NAL_H264["SPS"], NAL_H264["PPS"]}) if codec is Codec.H264 else HEVC_PARAM_SETS

    def _nal_type(self, nal: bytes) -> int:
        if self.codec is Codec.H264:
            return nal[0] & 0x1F
        return (nal[0] >> 1) & 0x3F

    def _is_keyframe(self, nal_types: Iterable[int]) -> bool:
        types = set(nal_types)
        if self.codec is Codec.H264:
            return bool(types & H264_KEYFRAME_NAL)
        return bool(types & HEVC_IRAP_NAL)

    def _nal_headers(self, data: bytes, end: int) -> list[tuple[int, int, int, int]]:
        """Return ``(start_code_offset, nal_offset, nal_type, payload_end)``."""
        result: list[tuple[int, int, int, int]] = []
        pos = 0
        next_code = _find_start_code(data, pos, end)
        while next_code is not None:
            sc_off, nal_off = next_code
            following = _find_start_code(data, nal_off, end)
            payload_end = following[0] if following is not None else end
            if nal_off < payload_end:
                if _forbidden_bit(data[nal_off]):
                    # an Annex B NAL header with the forbidden bit set is not
                    # a legal NAL: the start code was found inside random data
                    pos = payload_end if following is not None else end
                    next_code = following
                    continue
                nal_type = self._nal_type(data[nal_off:nal_off + 2])
                # payload ends before trailing zero bytes of the next start code
                trimmed_end = payload_end
                while trimmed_end > nal_off + 1 and data[trimmed_end - 1] == 0:
                    trimmed_end -= 1
                result.append((sc_off, nal_off, nal_type, trimmed_end))
            pos = payload_end if following is not None else end
            next_code = following
        return result

    def split(self, data: bytes, base_offset: int = 0, *, strict: bool = True) -> list[AccessUnit]:
        """Split a full in-memory stream into access units.

        The boundary rule is: start a new AU at an AUD NAL; otherwise at a VCL
        first-slice NAL when the current AU already contains a VCL slice.  A
        prefix NAL (SPS/PPS/SEI) encountered after a complete VCL slice also
        starts a new AU, so parameter sets travel with the frame that needs
        them.
        """
        nals = self._nal_headers(data, len(data))
        if not nals:
            raise make_error(ErrorCategory.STREAM_EMPTY, "no Annex B start codes found")
        # A trailing NAL whose start code is the final 3-4 bytes cannot be
        # distinguished from the beginning of the next NAL until more bytes
        # arrive; drop it during incremental parsing.
        if not strict and nals and nals[-1][1] + 2 >= len(data):
            nals = nals[:-1]
        # Group NAL headers into AUs; remember each AU's first start-code offset.
        groups: list[list[tuple[int, int, int, int]]] = []
        current: list[tuple[int, int, int, int]] = []
        has_vcl = False
        for entry in nals:
            nal_type = entry[2]
            is_aud = nal_type == self._aud
            is_vcl = nal_type in self._first_slice
            starts_new = False
            if current:
                starts_new = is_aud or (is_vcl and has_vcl) or (not is_vcl and not is_aud and has_vcl and nal_type in self._prefix)
            if starts_new:
                groups.append(current)
                current = []
                has_vcl = False
            current.append(entry)
            has_vcl = has_vcl or is_vcl
        if current:
            groups.append(current)

        aus: list[AccessUnit] = []
        for idx, group in enumerate(groups):
            unit_start = group[0][0]
            unit_end = groups[idx + 1][0][0] if idx + 1 < len(groups) else len(data)
            nal_types = tuple(entry[2] for entry in group)
            aus.append(
                AccessUnit(
                    index=idx,
                    offset=base_offset + unit_start,
                    size=unit_end - unit_start,
                    is_keyframe=self._is_keyframe(nal_types),
                    nal_types=nal_types,
                )
            )
        return aus


class Av1ObuParser:
    """Splits an AV1 low-overhead OBU stream into access units."""

    OBU_SEQUENCE_HEADER = 1
    OBU_TEMPORAL_DELIMITER = 2
    OBU_FRAME_HEADER = 3
    OBU_TILE_GROUP = 4
    OBU_FRAME = 6
    OBU_REDUNDANT_FRAME_HEADER = 7

    @staticmethod
    def _leb128(data: bytes, pos: int, end: int) -> tuple[int, int]:
        value = 0
        shift = 0
        while pos < end:
            byte = data[pos]
            pos += 1
            value |= (byte & 0x7F) << shift
            if not byte & 0x80:
                return value, pos
            shift += 7
            if shift > 56:
                raise make_error(ErrorCategory.STREAM_CORRUPT, "OBU size overflow", offset=pos)
        raise make_error(ErrorCategory.STREAM_TRUNCATED, "truncated OBU size field", offset=pos)

    def split(self, data: bytes, base_offset: int = 0, *, strict: bool = True) -> list[AccessUnit]:
        obus = self._scan(data, strict=strict)
        if not obus:
            raise make_error(ErrorCategory.STREAM_EMPTY, "no AV1 OBUs found")
        groups: list[list[tuple[int, int, int, int, int]]] = []
        current: list[tuple[int, int, int, int, int]] = []
        for entry in obus:
            if entry[0] == self.OBU_TEMPORAL_DELIMITER:
                if current:
                    groups.append(current)
                current = [entry]
            else:
                current.append(entry)
        if current:
            groups.append(current)
        aus: list[AccessUnit] = []
        for idx, group in enumerate(groups):
            start = group[0][1]
            end = group[-1][4]
            key = False
            for obu_type, _, _, payload_start, _ in group:
                if obu_type in (self.OBU_FRAME_HEADER, self.OBU_FRAME):
                    if payload_start < len(data):
                        header = data[payload_start]
                        frame_type = (header >> 5) & 0x03
                        if frame_type == 0:
                            key = True
                    break
            aus.append(
                AccessUnit(
                    index=idx,
                    offset=base_offset + start,
                    size=end - start,
                    is_keyframe=key,
                    nal_types=tuple(entry[0] for entry in group),
                )
            )
        return aus

    def _scan(self, data: bytes, *, strict: bool = True) -> list[tuple[int, int, int, int, int]]:
        """Return ``(type, obu_start, header_size, payload_start, obu_end)``.

        When ``strict`` is false a trailing OBU whose declared size exceeds the
        available bytes is dropped instead of raising; this is what the
        incremental scanner needs while more bytes may still arrive.
        """
        out: list[tuple[int, int, int, int, int]] = []
        pos = 0
        end = len(data)
        while pos < end:
            start = pos
            header = data[pos]
            pos += 1
            obu_type = (header >> 3) & 0x0F
            extension = bool(header & 0x04)
            has_size = bool(header & 0x02)
            if extension:
                if pos >= end:
                    if not strict:
                        break
                    raise make_error(ErrorCategory.STREAM_TRUNCATED, "truncated OBU extension", offset=pos)
                pos += 1
            if has_size:
                try:
                    size, pos = self._leb128(data, pos, end)
                except BitstreamError:
                    if not strict:
                        break
                    raise
            else:
                size = end - pos
            payload_start = pos
            if payload_start + size > end:
                if not strict:
                    break
                raise make_error(
                    ErrorCategory.STREAM_TRUNCATED,
                    "OBU size exceeds remaining bytes",
                    offset=start,
                    declared=size,
                    available=end - payload_start,
                )
            out.append((obu_type, start, pos - start, payload_start, payload_start + size))
            pos = payload_start + size
        return out


# ==========================================================================
# Incremental scanner
# ==========================================================================
