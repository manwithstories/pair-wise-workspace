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
