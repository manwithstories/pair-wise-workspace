"""Streaming raw-bitstream scanner for H.264, H.265 and AV1.

The scanner consumes an iterable of byte chunks of arbitrary size.  It keeps
only a bounded carry-over buffer between calls, so memory does not grow with
fixture duration, and it reports the absolute byte offset of every defect it
finds.  Completed access units (AUs) are yielded one at a time together with
their first-byte offset, so downstream stages can enqueue them with back
pressure instead of materialising the whole stream.

Defects recognised while scanning:

* a start code cut in half at a chunk boundary (resolved once data arrives),
* a start code with no NAL header, or a HEVC NAL header cut after one byte,
* a final access unit that is incomplete when the input ends,
* an AV1 OBU with a truncated length field or payload.

Parameter-set NALs are attached to the following picture, so every yielded AU
is self-contained enough for a decoder that is reset between AUs.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable, Iterator

from .errors import MediaCorruptError, MediaIssue

SUPPORTED_CODECS = ("h264", "h265", "av1")
#: Largest carry-over a scanner may hold while waiting for one AU (bytes).
MAX_AU_BYTES = 8 * 1024 * 1024


@dataclass
class AccessUnit:
    """One decodable picture (or AV1 temporal unit) found in the raw stream."""

    index: int
    offset: int
    data: bytes
    keyframe: bool
    is_irap: bool = False
    nal_types: tuple[int, ...] = ()
    truncated: bool = False
    complete: bool = True

    @property
    def size(self) -> int:
        return len(self.data)

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "offset": self.offset,
            "size": self.size,
            "keyframe": self.keyframe,
            "is_irap": self.is_irap,
            "nal_types": list(self.nal_types),
            "complete": self.complete,
        }


@dataclass
class ScanReport:
    """Stream-level facts gathered while scanning."""

    codec: str
    units: int = 0
    keyframes: int = 0
    bytes_consumed: int = 0
    truncated_tail_offset: int | None = None
    truncated_tail_reason: str = ""
    issues: list[MediaIssue] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "codec": self.codec,
            "units": self.units,
            "keyframes": self.keyframes,
            "bytes_consumed": self.bytes_consumed,
            "issues": [i.to_dict() for i in self.issues],
        }
        if self.truncated_tail_offset is not None:
            out["truncated_tail_offset"] = self.truncated_tail_offset
            out["truncated_tail_reason"] = self.truncated_tail_reason
        return out


class _ChunkReader:
    """Concatenating reader with bounded memory and absolute offsets."""

    def __init__(self, chunks: Iterable[bytes], limit: int = MAX_AU_BYTES) -> None:
        self._iter = iter(chunks)
        self._buffer = bytearray()
        self._base = 0  # absolute offset of buffer[0]
        self._eof = False
        self.limit = limit
        self.skipped = 0

    @property
    def position(self) -> int:
        """Absolute offset of the next unconsumed byte."""
        return self._base

    @property
    def offset(self) -> int:
        return self._base + len(self._buffer)

    def available(self) -> int:
        return len(self._buffer)

    def peek(self, start: int, n: int) -> bytes:
        return bytes(self._buffer[start:start + n])

    def consume(self, n: int) -> bytes:
        if n <= 0:
            return b""
        out = bytes(self._buffer[:n])
        del self._buffer[:n]
        self._base += n
        return out

    def discard(self, n: int) -> int:
        dropped = len(self.consume(n))
        self.skipped += dropped
        return dropped

    def fill(self, want: int = 1) -> bool:
        """Ensure at least ``want`` bytes are buffered; False at EOF."""
        while len(self._buffer) < want and not self._eof:
            try:
                chunk = next(self._iter)
            except StopIteration:
                self._eof = True
                break
            if chunk:
                self._buffer += chunk
                if len(self._buffer) > self.limit:
                    raise MediaCorruptError(
                        f"access unit exceeds {self.limit} byte budget",
                        offset=self._base, code="au_too_large")
        return len(self._buffer) >= want

    @property
    def eof(self) -> bool:
        return self._eof and not self._buffer


def _find_start_code(buf: bytes, start: int) -> int:
    """Index of the next 00 00 01 sequence at or after ``start``, or -1."""
    return buf.find(b"\x00\x00\x01", start)


def _nal_header(codec: str, byte: int) -> tuple[int, bool, bool]:
    """Return (nal_type, is_vcl, is_irap) for an H.264/H.265 NAL header byte."""
    if codec == "h264":
        nal_type = byte & 0x1F
        return nal_type, 1 <= nal_type <= 5, nal_type == 5
    nal_type = (byte >> 1) & 0x3F
    return nal_type, nal_type <= 31, 16 <= nal_type <= 23


def _aud_type(codec: str) -> int:
    return 9 if codec == "h264" else 35


def _parameter_types(codec: str) -> tuple[int, ...]:
    return (7, 8) if codec == "h264" else (32, 33, 34, 39)


def chunk_bytes(data: bytes, size: int) -> Iterator[bytes]:
    """Split bytes into fixed-size chunks (used by tests and the demo reader)."""
    if size <= 0:
        raise ValueError("chunk size must be positive")
    for start in range(0, len(data), size):
        yield data[start:start + size]


def _annexb_nals(data: bytes) -> Iterator[tuple[int, int, int]]:
    """Yield (start, payload_start, end) for each NAL in ``data``."""
    n = len(data)
    starts: list[int] = []
    pos = 0
    while pos + 3 <= n:
        idx = data.find(b"\x00\x00\x01", pos)
        if idx < 0:
            break
        start = idx - 1 if idx > 0 and data[idx - 1] == 0 else idx
        starts.append(start)
        pos = idx + 3
    for i, start in enumerate(starts):
        end = starts[i + 1] if i + 1 < len(starts) else n
        payload = start + (4 if data[start:start + 4] == b"\x00\x00\x00\x01" else 3)
        yield start, payload, end


def _read_start_code(reader: _ChunkReader, report: ScanReport) -> int | None:
    """Discard bytes up to one Annex-B ``00 00 01`` and consume it.

    The returned offset is the absolute position of the first zero of that
    three-byte prefix, which makes offsets independent of how the input was
    chunked.  A four-byte prefix (``00 00 00 01``) is accepted; its extra leading
    zero belongs to the bitstream's zero-padding, not to the NAL, so it is
    skipped silently.  Non-zero bytes before a start code are reported once.
    """
    garbage_at: int | None = None
    while True:
        if not reader.fill(3):
            leftover = reader.available()
            if leftover:
                if any(b != 0 for b in reader.peek(0, leftover)):
                    if garbage_at is None:
                        garbage_at = reader.position
                reader.discard(leftover)
            if garbage_at is not None:
                report.issues.append(MediaIssue(
                    "trailing_garbage", "non-zero bytes after the last NAL were ignored",
                    garbage_at, severity="warning"))
            return None
        buf = reader.peek(0, reader.available())
        idx = _find_start_code(buf, 0)
        if idx >= 0:
            prefix = buf[:idx]
            if any(b != 0 for b in prefix):
                garbage_at = reader.position
                report.issues.append(MediaIssue(
                    "garbage_skipped", "non-NAL bytes were skipped",
                    garbage_at, severity="warning",
                    detail={"bytes": idx}))
            reader.discard(idx)
            offset = reader.position
            reader.discard(3)
            return offset
        keep = 2 if buf.endswith(b"\x00\x00") else (1 if buf.endswith(b"\x00") else 0)
        drop = len(buf) - keep
        if drop > 0:
            chunk = reader.peek(0, drop)
            if any(b != 0 for b in chunk) and garbage_at is None:
                garbage_at = reader.position
            reader.discard(drop)
        elif not reader.fill(reader.available() + 1):
            leftover = reader.available()
            if leftover:
                if any(b != 0 for b in reader.peek(0, leftover)):
                    if garbage_at is None:
                        garbage_at = reader.position
                reader.discard(leftover)
            if garbage_at is not None:
                report.issues.append(MediaIssue(
                    "trailing_garbage", "non-zero bytes after the last NAL were ignored",
                    garbage_at, severity="warning"))
            return None


def _read_nal_body(reader: _ChunkReader) -> tuple[bytes, bool]:
    """Read payload up to (not including) the next ``00 00 01`` start code.

    Returns the payload and whether EOF was reached.  Two trailing bytes are
    always retained between chunks so a start code split across a chunk boundary
    is never treated as payload.  Any zero-padding in front of the next start
    code is left for the start-code reader, keeping NAL boundaries independent
    of chunking.
    """
    body = bytearray()
    while True:
        buf = reader.peek(0, reader.available())
        idx = _find_start_code(bytes(buf), 0)
        if idx >= 0:
            body += reader.consume(idx)
            return bytes(body), False
        if reader.eof:
            body += reader.consume(reader.available())
            return bytes(body), True
        if reader.available() <= 2:
            if not reader.fill(reader.available() + 1):
                body += reader.consume(reader.available())
                return bytes(body), True
            continue
        body += reader.consume(reader.available() - 2)


def scan_annexb(chunks: Iterable[bytes], *, codec: str) -> Iterator[AccessUnit | ScanReport]:
    """Yield access units from an Annex-B byte stream, then a final ScanReport.

    An AU starts at its access-unit delimiter (AUD) when one is present.  Without
    an AUD the first NAL starts the unit, and a new AUD or a repeated parameter
    set after a picture closes it.  Parameter sets are prefixed to the picture
    they precede, so each yielded unit can be decoded by a fresh context.
    Offsets are absolute file offsets and do not depend on chunking.
    """
    if codec not in ("h264", "hevc"):
        raise ValueError(f"unsupported annex-b codec {codec!r}")
    reader = _ChunkReader(chunks)
    report = ScanReport(codec=codec)
    aud_type = _aud_type(codec)
    param_types = _parameter_types(codec)
    units: list[AccessUnit] = []

    current: list[bytes] = []
    nal_types: list[int] = []
    key = irap = have_vcl = False
    unit_offset = 0
    tail_truncated = False
    tail_reason = ""

    def close(complete: bool, reason: str = "") -> None:
        nonlocal current, nal_types, key, irap, have_vcl, unit_offset
        if not current:
            return
        unit = AccessUnit(
            index=report.units, offset=unit_offset,
            data=b"".join(b"\x00\x00\x01" + nal for nal in current),
            keyframe=key, is_irap=irap, nal_types=tuple(nal_types),
            complete=complete, truncated=not complete)
        report.units += 1
        report.keyframes += int(key)
        if not complete:
            report.truncated_tail_offset = unit_offset
            report.truncated_tail_reason = reason
            report.issues.append(MediaIssue(
                "truncated_tail", reason, unit_offset, detail={"size": unit.size}))
        report.issues.extend(_validate_au(codec, unit))
        units.append(unit)
        current, nal_types = [], []
        key = irap = have_vcl = False

    while True:
        offset = _read_start_code(reader, report)
        if offset is None:
            break
        body, at_eof = _read_nal_body(reader)
        if not body:
            report.issues.append(MediaIssue(
                "truncated_start_code", "start code with no NAL payload", offset))
            if at_eof:
                tail_truncated = True
                tail_reason = "stream ends inside a start code"
            continue
        if codec == "hevc" and len(body) < 2:
            report.issues.append(MediaIssue(
                "truncated_nal_header", "HEVC NAL header needs two bytes", offset))
            if at_eof:
                tail_truncated = True
                tail_reason = "stream ends inside an HEVC NAL header"
            continue
        nal_type, is_vcl, is_irap = _nal_header(codec, body[0])
        if not current:
            unit_offset = offset
        elif have_vcl and (nal_type == aud_type or nal_type in param_types):
            close(True)
            unit_offset = offset
        current.append(body)
        nal_types.append(nal_type)
        if is_vcl:
            have_vcl = True
            key = key or is_irap
            irap = irap or is_irap
        if at_eof:
            complete, reason = _tail_complete(codec, body, is_vcl)
            if not complete:
                tail_truncated = True
                tail_reason = reason
            break

    if current:
        close(not tail_truncated, tail_reason)
    for unit in units:
        yield unit
    report.bytes_consumed = reader.offset
    report.skipped = reader.skipped
    yield report


def _validate_au(codec: str, unit: AccessUnit) -> list[MediaIssue]:
    """Structural checks that need no decode: headers and start codes."""
    issues: list[MediaIssue] = []
    data = unit.data
    if not data:
        return [MediaIssue("empty_au", "empty access unit", unit.offset)]
    n = len(data)
    for start, payload, end in _annexb_nals(data):
        if payload >= end:
            issues.append(MediaIssue(
                "truncated_start_code", "start code with no NAL payload",
                unit.offset + start))
            continue
        if codec == "hevc" and payload + 1 >= end:
            issues.append(MediaIssue(
                "truncated_nal_header", "HEVC NAL header needs two bytes",
                unit.offset + start))
    return issues


def _tail_complete(codec: str, body: bytes, is_vcl: bool) -> tuple[bool, str]:
    """Structural completeness of the final NAL (and the final access unit)."""
    if not body:
        return False, "stream ends immediately after a start code"
    if codec == "hevc" and len(body) < 2:
        return False, "stream ends inside an HEVC NAL header"
    if is_vcl and len(body) < 3:
        return False, "VCL NAL header truncated"
    return True, ""


def scan_av1(chunks: Iterable[bytes]) -> Iterator[AccessUnit | ScanReport]:
    """Yield AV1 temporal units (one per shown frame) then a final ScanReport.

    Each OBU length is checked against the remaining input, so a cut payload is
    reported at the OBU's own offset instead of yielding a silently shortened
    frame.  A temporal delimiter (OBU type 2) closes the previous unit and starts
    the next, so the offsets remain stable for any chunk size.
    """
    reader = _ChunkReader(chunks)
    report = ScanReport(codec="av1")
    parts: list[bytes] = []
    types: list[int] = []
    key = False
    unit_offset = 0
    units: list[AccessUnit] = []
    tail_truncated = False
    tail_reason = ""

    def close(complete: bool, reason: str = "") -> AccessUnit | None:
        nonlocal parts, types, key, unit_offset
        if not parts:
            return None
        unit = AccessUnit(
            index=report.units, offset=unit_offset, data=b"".join(parts),
            keyframe=key, is_irap=key, nal_types=tuple(types),
            complete=complete, truncated=not complete)
        report.units += 1
        report.keyframes += int(key)
        if not complete:
            report.truncated_tail_offset = unit_offset
            report.truncated_tail_reason = reason
            report.issues.append(MediaIssue(
                "truncated_av1_unit", reason, unit_offset,
                detail={"obu_types": types[-4:], "size": unit.size}))
        units.append(unit)
        parts, types, key = [], [], False
        return unit

    def read_leb128(pos: int) -> int | None:
        """Read a leb128 size starting at buffer offset ``pos``."""
        value = 0
        shift = 0
        while True:
            if not reader.fill(pos + 1):
                return None
            byte = reader.peek(pos, 1)[0]
            pos += 1
            value |= (byte & 0x7F) << shift
            shift += 7
            if not byte & 0x80:
                return value, pos
            if shift > 56:
                return None

    while True:
        if not reader.fill(1):
            break
        if not parts:
            unit_offset = reader.position
        if not reader.fill(2):
            tail_truncated = True
            tail_reason = "stream ends inside an OBU header"
            report.issues.append(MediaIssue(
                "truncated_obu_header", tail_reason, reader.position))
            break
        header = reader.peek(0, 1)[0]
        if header & 0x80:
            report.issues.append(MediaIssue(
                "obu_forbidden_bit", "OBU forbidden bit set; resynchronised by one byte",
                reader.position))
            parts.append(reader.consume(1))
            continue
        obu_type = (header >> 3) & 0x0F
        has_size = bool((header >> 1) & 0x01)
        has_ext = bool((header >> 2) & 0x01)
        header_len = 1 + has_ext
        if not reader.fill(header_len + 1):
            tail_truncated = True
            tail_reason = "stream ends inside an OBU header"
            report.issues.append(MediaIssue(
                "truncated_obu_header", tail_reason, reader.position))
            break
        if has_size:
            parsed = read_leb128(header_len)
            if parsed is None:
                tail_truncated = True
                tail_reason = "stream ends inside an OBU length field"
                report.issues.append(MediaIssue(
                    "truncated_obu_length", tail_reason, reader.position))
                break
            size, body_at = parsed
            frame_end = body_at + size
            if not reader.fill(frame_end):
                tail_truncated = True
                tail_reason = "stream ends inside an OBU payload"
                report.issues.append(MediaIssue(
                    "truncated_obu_payload", tail_reason, reader.position,
                    detail={"obu_type": obu_type, "declared": size,
                            "available": max(0, reader.available() - body_at)}))
                parts.append(reader.consume(reader.available()))
                types.append(obu_type)
                break
        else:
            # A size-less OBU (only legal last in a temporal unit) runs to EOF.
            body_at = header_len
            frame_end = -1
        obu = reader.consume(frame_end if frame_end >= 0 else reader.available())
        parts.append(obu)
        types.append(obu_type)
        if obu_type in (3, 6):
            body = obu[body_at:]
            if body and not body[0] >> 7:
                frame_type = (body[0] >> 5) & 0x03
                if frame_type == 0:
                    key = True
        if obu_type == 2:
            # The delimiter belongs to the unit it opens: hold it back.
            delimiter = parts.pop()
            types.pop()
            close(True)
            parts, types = [delimiter], [2]
            unit_offset = reader.position - len(delimiter)

    if parts:
        close(not tail_truncated, tail_reason)
    for unit in units:
        yield unit
    report.bytes_consumed = reader.position
    report.skipped = reader.skipped
    yield report


def scan(chunks: Iterable[bytes], *, codec: str) -> Iterator[AccessUnit | ScanReport]:
    """Dispatch to the raw-bitstream scanner for ``codec``."""
    codec = codec.lower()
    if codec in ("h264", "avc"):
        return scan_annexb(chunks, codec="h264")
    if codec in ("h265", "hevc"):
        return scan_annexb(chunks, codec="hevc")
    if codec == "av1":
        return scan_av1(chunks)
    raise ValueError(f"unsupported codec {codec!r}; expected one of {SUPPORTED_CODECS}")
