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


def scan(chunks: Iterable[bytes], *, codec: str) -> Iterator[AccessUnit | ScanReport]:
    """Dispatch to the raw-bitstream scanner for ``codec``."""
    codec = codec.lower()
    if codec in ("h264", "avc"):
        return scan_annexb(chunks, codec="h264")
    if codec in ("h265", "hevc"):
        return scan_annexb(chunks, codec="h265")
    if codec == "av1":
        return scan_av1(chunks)
    raise ValueError(f"unsupported codec {codec!r}; expected one of {SUPPORTED_CODECS}")
