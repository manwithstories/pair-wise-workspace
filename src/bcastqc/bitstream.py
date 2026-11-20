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


class StreamScanner:
    """Incrementally splits a raw stream fed in arbitrary byte chunks.

    ``feed()`` returns only *complete* access units.  At most ``max_buffer``
    bytes are retained between feeds; exceeding that limit raises a resource
    error instead of growing without bound.  Call :meth:`finish` at EOF to get
    the final unit (which, for truncated streams, is flagged and reported
    through :class:`BitstreamError` only when decoding fails).
    """

    def __init__(self, codec: Codec, max_buffer: int = 8 * 1024 * 1024) -> None:
        self.codec = codec
        self.max_buffer = max_buffer
        self._parser: AnnexBParser | Av1ObuParser = (
            AnnexBParser(codec) if codec in (Codec.H264, Codec.H265) else Av1ObuParser()
        )
        self._buffer = bytearray()
        self._buffer_offset = 0  # absolute offset of _buffer[0]
        self._next_index = 0
        self._emitted_end = 0  # absolute offset of first unemitted byte
        self._last_split: list[AccessUnit] | None = None
        self._last_split_len = 0
        self._truncated_tail = False

    @property
    def buffer_bytes(self) -> int:
        return len(self._buffer)

    def feed(self, chunk: bytes) -> Iterator[AccessUnit]:
        if not chunk:
            return
        self._buffer.extend(chunk)
        if len(self._buffer) > self.max_buffer:
            raise make_error(
                ErrorCategory.LIMIT_EXCEEDED,
                "access unit larger than scanner buffer",
                limit=self.max_buffer,
                buffered=len(self._buffer),
            )
        yield from self._drain(final=False)

    def finish(self) -> Iterator[AccessUnit]:
        yield from self._drain(final=True)
        if self._buffer:
            raise make_error(
                ErrorCategory.STREAM_TRUNCATED,
                "trailing bytes do not form a complete access unit",
                offset=self._buffer_offset,
                trailing=len(self._buffer),
            )

    def _drain(self, final: bool) -> Iterator[AccessUnit]:
        try:
            units = self._parser.split(bytes(self._buffer), self._buffer_offset, strict=final)
        except BitstreamError:
            if final:
                raise
            units = []
        # All units except the last are complete; the last is complete only at EOF.
        ready = units if final else units[:-1]
        for unit in ready:
            if unit.offset < self._emitted_end:
                continue  # already emitted in a previous feed
            self._emitted_end = unit.end
            adjusted = AccessUnit(
                index=self._next_index,
                offset=unit.offset,
                size=unit.size,
                is_keyframe=unit.is_keyframe,
                nal_types=unit.nal_types,
            )
            self._next_index += 1
            yield adjusted
        consumed = self._emitted_end - self._buffer_offset
        if consumed > 0:
            del self._buffer[:consumed]
            self._buffer_offset = self._emitted_end


# ==========================================================================
# Keyframe index
# ==========================================================================


@dataclass(frozen=True)
class KeyframeEntry:
    offset: int
    size: int = 0
    index: int | None = None
    timestamp: int | None = None

    def to_json(self) -> dict:
        payload = {"offset": self.offset, "size": self.size}
        if self.index is not None:
            payload["index"] = self.index
        if self.timestamp is not None:
            payload["timestamp"] = self.timestamp
        return payload


@dataclass(frozen=True)
class IndexIssue:
    kind: str  # duplicate | out_of_order | out_of_range | malformed
    entry_position: int  # position in the submitted index (0-based)
    offset: int
    message: str
    source_byte: int | None = None  # byte position in the index file
    recoverable: bool = True

    def to_json(self) -> dict:
        payload = {
            "kind": self.kind,
            "entry_position": self.entry_position,
            "offset": self.offset,
            "message": self.message,
            "recoverable": self.recoverable,
        }
        if self.source_byte is not None:
            payload["source_byte"] = self.source_byte
        return payload


@dataclass
class KeyframeIndex:
    entries: list[KeyframeEntry]
    raw_text: str | None = None

    @classmethod
    def from_json(cls, payload: object, raw_text: str | None = None) -> "KeyframeIndex":
        if isinstance(payload, dict):
            if "keyframes" in payload:
                payload = payload["keyframes"]
            elif "index" in payload:
                payload = payload["index"]
        if not isinstance(payload, list):
            raise make_error(ErrorCategory.INDEX_MALFORMED, "keyframe index must be a list")
        entries: list[KeyframeEntry] = []
        for pos, item in enumerate(payload):
            if isinstance(item, int):
                entries.append(KeyframeEntry(offset=item))
                continue
            if not isinstance(item, dict) or "offset" not in item:
                raise make_error(
                    ErrorCategory.INDEX_MALFORMED,
                    "index entry must be an integer or an object with an offset",
                    entry_position=pos,
                )
            offset = item["offset"]
            size = item.get("size", 0)
            if not isinstance(offset, int) or not isinstance(size, int) or offset < 0 or size < 0:
                raise make_error(
                    ErrorCategory.INDEX_MALFORMED,
                    "offset and size must be non-negative integers",
                    entry_position=pos,
                    offset=offset,
                    size=size,
                )
            entries.append(KeyframeEntry(offset=offset, size=size, index=item.get("index"), timestamp=item.get("timestamp")))
        return cls(entries=entries, raw_text=raw_text)

    @classmethod
    def from_file(cls, path: Path) -> "KeyframeIndex":
        import json

        text = path.read_text(encoding="utf-8")
        try:
            payload = json.loads(text)
        except json.JSONDecodeError as exc:
            raise make_error(
                ErrorCategory.INDEX_MALFORMED,
                f"index is not valid JSON: {exc.msg}",
                line=exc.lineno,
                column=exc.colno,
            ) from exc
        return cls.from_json(payload, raw_text=text)

    def to_json(self) -> list[dict]:
        return [entry.to_json() for entry in self.entries]


@dataclass
class IndexValidation:
    accepted: bool
    issues: list[IndexIssue]
    recovered_index: KeyframeIndex | None = None
    reasons: list[str] = field(default_factory=list)

    @property
    def rejected(self) -> bool:
        return not self.accepted

    def to_json(self) -> dict:
        payload = {
            "accepted": self.accepted,
            "issues": [issue.to_json() for issue in self.issues],
            "reasons": list(self.reasons),
        }
        if self.recovered_index is not None:
            payload["recovered_entries"] = len(self.recovered_index.entries)
        return payload


def validate_index(
    index: KeyframeIndex,
    stream_size: int,
    *,
    keyframes: Iterable[AccessUnit] | None = None,
    policy: str = "scan",
) -> IndexValidation:
    """Validate a keyframe index against the real stream.

    ``policy`` is ``"scan"`` (recover by scanning the stream when possible) or
    ``"reject"`` (any structural issue invalidates the index).  Out-of-range
    offsets are never recoverable through the index itself; they only become
    usable by scanning.
    """
    issues: list[IndexIssue] = []
    last_offset = -1
    seen: dict[int, int] = {}
    for pos, entry in enumerate(index.entries):
        if entry.offset >= stream_size:
            issues.append(
                IndexIssue(
                    kind="out_of_range",
                    entry_position=pos,
                    offset=entry.offset,
                    message=f"offset {entry.offset} is outside the {stream_size}-byte stream",
                    source_byte=entry.offset,
                    recoverable=True,
                )
            )
        if entry.size and entry.offset + entry.size > stream_size:
            issues.append(
                IndexIssue(
                    kind="out_of_range",
                    entry_position=pos,
                    offset=entry.offset,
                    message=f"entry end {entry.offset + entry.size} exceeds stream size {stream_size}",
                    source_byte=entry.offset,
                    recoverable=True,
                )
            )
        if entry.offset in seen:
            issues.append(
                IndexIssue(
                    kind="duplicate",
                    entry_position=pos,
                    offset=entry.offset,
                    message=f"duplicate offset {entry.offset} (first at entry {seen[entry.offset]})",
                    source_byte=entry.offset,
                    recoverable=True,
                )
            )
        else:
            seen[entry.offset] = pos
        if entry.offset < last_offset:
            issues.append(
                IndexIssue(
                    kind="out_of_order",
                    entry_position=pos,
                    offset=entry.offset,
                    message=f"offset {entry.offset} follows larger offset {last_offset}",
                    source_byte=entry.offset,
                    recoverable=True,
                )
            )
        last_offset = max(last_offset, entry.offset)

    if not issues:
        if keyframes is not None:
            known = {au.offset for au in keyframes if au.is_keyframe}
            for pos, entry in enumerate(index.entries):
                if entry.offset not in known:
                    issues.append(
                        IndexIssue(
                            kind="not_keyframe",
                            entry_position=pos,
                            offset=entry.offset,
                            message=f"offset {entry.offset} is not a keyframe access unit",
                            source_byte=entry.offset,
                            recoverable=True,
                        )
                    )
        if not issues:
            return IndexValidation(accepted=True, issues=[])

    fatal = [issue for issue in issues if not issue.recoverable]
    if policy == "reject" or fatal:
        return IndexValidation(
            accepted=False,
            issues=issues,
            reasons=[issue.message for issue in issues],
        )
    if keyframes is None:
        return IndexValidation(
            accepted=False,
            issues=issues,
            reasons=["index issues require a stream scan to recover"],
        )
    recovered = compute_index(list(keyframes))
    return IndexValidation(
        accepted=True,
        issues=issues,
        recovered_index=recovered,
        reasons=[f"recovered by scanning: {len(recovered.entries)} keyframes"],
    )


def compute_index(access_units: Iterable[AccessUnit]) -> KeyframeIndex:
    """Build a canonical keyframe index from scanned access units."""
    entries = [
        KeyframeEntry(offset=au.offset, size=au.size, index=au.index, timestamp=au.timestamp)
        for au in access_units
        if au.is_keyframe
    ]
    return KeyframeIndex(entries=entries)


def file_digest_slices(path: Path, *, chunk_size: int = 1 << 20) -> tuple[str, int]:
    """Return ``(sha256 hex, size)`` of a file without loading it into memory."""
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as handle:
        while True:
            block = handle.read(chunk_size)
            if not block:
                break
            digest.update(block)
            size += len(block)
    return digest.hexdigest(), size


def index_digest(index: KeyframeIndex) -> str:
    """Stable digest of index geometry (offsets and sizes only)."""
    digest = hashlib.sha256()
    for entry in index.entries:
        digest.update(f"{entry.offset}:{entry.size};".encode())
    return digest.hexdigest()


# ==========================================================================
# Access-unit conformance
# ==========================================================================


class _BitReader:
    """Minimal MSB-first bit reader for slice-header prefix checks."""

    def __init__(self, data: bytes) -> None:
        self._data = data
        self._pos = 0

    def bit(self) -> int | None:
        byte_index, bit_index = divmod(self._pos, 8)
        if byte_index >= len(self._data):
            return None
        self._pos += 1
        return (self._data[byte_index] >> (7 - bit_index)) & 1

    def ue(self) -> int | None:
        zeros = 0
        while True:
            value = self.bit()
            if value is None:
                return None
            if value:
                break
            zeros += 1
            if zeros > 31:
                return None
        suffix = 0
        for _ in range(zeros):
            value = self.bit()
            if value is None:
                return None
            suffix = (suffix << 1) | value
        return (1 << zeros) - 1 + suffix


def _h264_slice_prefix_problem(rbsp: bytes) -> str | None:
    """Check ``first_mb_in_slice == 0`` and a legal ``slice_type``."""
    reader = _BitReader(rbsp)
    first_mb = reader.ue()
    if first_mb is None:
        return "slice header truncated"
    if first_mb != 0:
        return f"first_mb_in_slice is {first_mb} (expected 0)"
    slice_type = reader.ue()
    if slice_type is None:
        return "slice_type missing"
    if slice_type > 9:
        return f"illegal slice_type {slice_type}"
    return None


def _hevc_slice_prefix_problem(rbsp: bytes) -> str | None:
    """Check ``first_slice_segment_in_pic_flag == 1``."""
    if not rbsp:
        return "slice header truncated"
    if not (rbsp[0] & 0x80):
        return "first_slice_segment_in_pic_flag is 0"
    return None


def validate_access_unit(codec: Codec, payload: bytes, *, strict: bool = False) -> str | None:
    """Cheap structural conformance check on a single access unit's bytes.

    Returns a short reason string when the unit is malformed, or ``None``.
    This is deliberately syntactic - it catches the damage a bitstream
    parser can see without decoding (truncated NAL headers, NAL length
    overruns, missing slice data, OBU size overruns), which is what lets the
    pipeline report an illegal access unit even when the decoder would
    silently emit nothing.

    With ``strict=True`` an access unit that contains no VCL / frame payload
    is also rejected; ``strict=False`` accepts parameter-set-only units, which
    are legal at the start of a group.
    """
    if not payload:
        return "empty access unit"
    if codec in (Codec.H264, Codec.H265):
        parser = AnnexBParser(codec)
        first = _first_nal_offset(payload)
        if first is None:
            return "no Annex B start code"
        if first >= len(payload) or _forbidden_bit(payload[first]):
            return "forbidden_zero_bit set"
        headers = parser._nal_headers(payload, len(payload))
        if not headers:
            return "no Annex B start code"
        vcl = (
            H264_FIRST_SLICE_NAL if codec is Codec.H264
            else frozenset(range(0, 10)) | frozenset(range(16, 22))
        )
        has_vcl = False
        checked_prefix = False
        for start, nal_off, nal_type, payload_end in headers:
            if nal_off + 1 > payload_end:
                return f"truncated NAL header at {start}"
            if nal_type in vcl:
                has_vcl = True
                # a slice NAL carries at least a 1-byte header, 1 byte of slice
                # header and some macroblock data; a handful of bytes means the
                # unit was cut or corrupted
                if payload_end - nal_off < 8:
                    return f"slice NAL too short at {start} ({payload_end - nal_off} bytes)"
                if not checked_prefix:
                    checked_prefix = True
                    header_len = 1 if codec is Codec.H264 else 2
                    rbsp = payload[nal_off + header_len:payload_end]
                    if codec is Codec.H264:
                        problem = _h264_slice_prefix_problem(rbsp)
                    else:
                        problem = _hevc_slice_prefix_problem(rbsp)
                    if problem is not None:
                        return f"{problem} at {start}"
        if strict and not has_vcl:
            return "access unit has no slice payload"
        return None
    # AV1 low-overhead OBU stream
    parser = Av1ObuParser()
    try:
        obus = parser._scan(payload, strict=True)
    except BitstreamError as exc:
        return str(exc)
    if not obus:
        return "no AV1 OBUs"
    if strict and not any(entry[0] in (Av1ObuParser.OBU_FRAME, Av1ObuParser.OBU_FRAME_HEADER) for entry in obus):
        return "access unit has no frame OBU"
    return None
