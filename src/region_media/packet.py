"""Edge capture parsing: record framing, RTP headers and packet bookkeeping.

The edge nodes write a small self-describing capture instead of a live socket
dump so that damaged input is still recoverable::

    record := MAGIC(4)=b"EDGE" | kind(u8) | flags(u8) | reserved(u16)
              | length(u32 BE) | crc32(u32 BE) | payload[length]

Three record kinds are understood:

* ``KIND_RTP``   - one RTP packet, payload = u16 declared samples, u16 pad,
                   interleaved little-endian float32 PCM
* ``KIND_RTCP``  - one compound RTCP packet (sender reports etc.)
* ``KIND_VIDEO`` - one video record, payload = UTF-8 JSON metadata

Framing is deliberately redundant (magic + declared length + CRC) so a record
truncated by a failed disk write can be detected and skipped while the reader
resynchronises on the next magic marker.  This is what lets a truncated packet
be *recorded as an error* and the following valid packets still be processed.
"""

from __future__ import annotations

import json
import struct
import zlib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterator

import numpy as np

from .errors import Code, Diagnostic

MAGIC = b"EDGE"
_HEADER = struct.Struct(">4sBBHII")
HEADER_SIZE = _HEADER.size  # 16
MAX_RECORD = 16 * 1024 * 1024

KIND_RTP = 1
KIND_RTCP = 2
KIND_VIDEO = 3

_KNOWN_KINDS = {KIND_RTP, KIND_RTCP, KIND_VIDEO}


class CaptureFormatError(Exception):
    """Raised when the capture file itself cannot be opened at all."""


@dataclass
class StreamSpec:
    """Description of the captured RTP stream (the edge's SDP equivalent)."""

    sample_rate: int = 48000
    channels: int = 2
    layout: str = "stereo"
    clock_rate: int = 48000
    ssrc: int = 0x0
    payload_type: int = 96
    nominal_samples: int = 480

    @property
    def frame_bytes(self) -> int:
        return 4 * self.channels

    def to_json(self) -> dict[str, Any]:
        return {
            "version": 1,
            "ssrc": self.ssrc,
            "payload_type": self.payload_type,
            "clock_rate": self.clock_rate,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "layout": self.layout,
            "nominal_samples": self.nominal_samples,
            "capture": "capture.edg",
        }

    @classmethod
    def from_json(cls, data: dict[str, Any]) -> "StreamSpec":
        try:
            return cls(
                sample_rate=int(data.get("sample_rate", 48000)),
                channels=int(data.get("channels", 2)),
                layout=str(data.get("layout", "stereo")),
                clock_rate=int(data.get("clock_rate", data.get("sample_rate", 48000))),
                ssrc=int(data.get("ssrc", 0)),
                payload_type=int(data.get("payload_type", 96)),
                nominal_samples=int(data.get("nominal_samples", 480)),
            )
        except (TypeError, ValueError) as exc:
            raise CaptureFormatError(f"invalid stream description: {exc}") from exc

    @classmethod
    def load(cls, path: Path) -> "StreamSpec":
        try:
            raw = path.read_text(encoding="utf-8")
        except OSError as exc:
            raise CaptureFormatError(f"cannot read {path}: {exc}") from exc
        try:
            data = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise CaptureFormatError(f"{path} is not valid JSON: {exc}") from exc
        if not isinstance(data, dict):
            raise CaptureFormatError(f"{path} must contain a JSON object")
        return cls.from_json(data)


@dataclass
class CaptureRecord:
    kind: int
    flags: int
    payload: bytes
    offset: int
    index: int


def write_record(kind: int, payload: bytes, *, flags: int = 0) -> bytes:
    """Encode one capture record (used by the fixture generator and tests)."""
    header = _HEADER.pack(
        MAGIC, kind, flags, 0, len(payload), zlib.crc32(payload) & 0xFFFFFFFF
    )
    return header + payload


def write_truncated_record(
    kind: int, payload: bytes, keep: int, *, flags: int = 0
) -> bytes:
    """Encode a record whose declared length exceeds the bytes actually written.

    Models an interrupted disk flush: the header (with the original length and
    CRC) lands, but only ``keep`` payload bytes make it to the file.
    """
    if not 0 <= keep < len(payload):
        raise ValueError("keep must be smaller than the payload")
    header = _HEADER.pack(
        MAGIC, kind, flags, 0, len(payload), zlib.crc32(payload) & 0xFFFFFFFF
    )
    return header + payload[:keep]


def read_capture(
    path: Path,
) -> Iterator[tuple[CaptureRecord | None, list[Diagnostic]]]:
    """Yield ``(record, diagnostics)`` pairs from a capture file.

    ``record`` is ``None`` when a damaged span was skipped; the diagnostics
    explain why.  The reader never raises for damaged content - callers get a
    best-effort stream plus structured errors.
    """
    try:
        data = path.read_bytes()
    except OSError as exc:
        raise CaptureFormatError(f"cannot read {path}: {exc}") from exc
    pos = 0
    index = 0
    total = len(data)
    while pos < total:
        found = data.find(MAGIC, pos)
        if found < 0:
            yield None, [
                Diagnostic(
                    Code.RECORD_RESYNC,
                    "trailing bytes without a record header",
                    str(path),
                    {"offset": pos, "bytes": total - pos},
                )
            ]
            return
        if found != pos:
            yield None, [
                Diagnostic(
                    Code.RECORD_RESYNC,
                    "skipped bytes before the next record",
                    str(path),
                    {"offset": pos, "skipped": found - pos},
                )
            ]
            pos = found
        if total - pos < HEADER_SIZE:
            yield None, [
                Diagnostic(
                    Code.TRUNCATED_RECORD,
                    "record header cut off at end of capture",
                    str(path),
                    {"offset": pos, "remaining": total - pos},
                )
            ]
            return
        magic, kind, flags, _reserved, length, checksum = _HEADER.unpack_from(
            data, pos
        )
        assert magic == MAGIC
        if kind not in _KNOWN_KINDS or length > MAX_RECORD:
            yield None, [
                Diagnostic(
                    Code.MALFORMED_HEADER,
                    "record header has an invalid kind or length",
                    str(path),
                    {"offset": pos, "kind": kind, "length": length},
                )
            ]
            # Skip this magic marker and look for the next one.
            pos += len(MAGIC)
            continue
        start = pos + HEADER_SIZE
        end = start + length
        if end > total:
            yield None, [
                Diagnostic(
                    Code.TRUNCATED_RECORD,
                    "record payload cut off at end of capture",
                    str(path),
                    {
                        "offset": pos,
                        "declared": length,
                        "available": total - start,
                    },
                )
            ]
            return
        payload = data[start:end]
        if (zlib.crc32(payload) & 0xFFFFFFFF) != checksum:
            # A short write leaves the next record's magic inside the span we
            # were told to read; resynchronise there.
            nxt = data.find(MAGIC, start)
            truncated_at = nxt if 0 <= nxt < end else None
            diag = Diagnostic(
                Code.TRUNCATED_RECORD,
                "record payload is shorter than declared (CRC mismatch)",
                str(path),
                {
                    "offset": pos,
                    "declared": length,
                    "actual": (truncated_at - start)
                    if truncated_at is not None
                    else length,
                },
            )
            if truncated_at is None:
                yield None, [diag]
                pos = end
                continue
            yield None, [diag]
            pos = truncated_at
            continue
        yield CaptureRecord(kind, flags, payload, pos, index), []
        index += 1
        pos = end


@dataclass
class RtpHeader:
    version: int
    padding: bool
    extension: bool
    csrc_count: int
    marker: bool
    payload_type: int
    sequence: int
    timestamp: int
    ssrc: int
    header_bytes: int
    padding_bytes: int


def parse_rtp_header(payload: bytes) -> RtpHeader:
    """Parse a fixed RTP header (RFC 3550 5.1).

    Raises :class:`ValueError` on a short or structurally invalid header; the
    caller turns that into ``ERR_MALFORMED_HEADER`` and moves on.
    """
    if len(payload) < 12:
        raise ValueError(f"RTP header truncated ({len(payload)} bytes)")
    b0, b1, seq, ts, ssrc = struct.unpack_from("!BBHII", payload, 0)
    version = b0 >> 6
    if version != 2:
        raise ValueError(f"unsupported RTP version {version}")
    csrc_count = b0 & 0x0F
    extension = bool(b0 & 0x10)
    padding = bool(b0 & 0x20)
    header_bytes = 12 + 4 * csrc_count
    if extension:
        if len(payload) < header_bytes + 4:
            raise ValueError("RTP extension header truncated")
        ext_words = struct.unpack_from("!H", payload, header_bytes + 2)[0]
        header_bytes += 4 + 4 * ext_words
    if header_bytes > len(payload):
        raise ValueError("RTP header longer than packet")
    padding_bytes = 0
    if padding:
        padding_bytes = payload[-1]
        if padding_bytes == 0 or header_bytes + padding_bytes > len(payload):
            raise ValueError("invalid RTP padding")
    return RtpHeader(
        version=version,
        padding=padding,
        extension=extension,
        csrc_count=csrc_count,
        marker=bool(b1 & 0x80),
        payload_type=b1 & 0x7F,
        sequence=seq,
        timestamp=ts,
        ssrc=ssrc,
        header_bytes=header_bytes,
        padding_bytes=padding_bytes,
    )


@dataclass
class AudioPayload:
    declared_samples: int
    samples: np.ndarray  # (n, channels) float32
    truncated: bool
    actual_samples: int


def decode_audio_payload(
    body: bytes, channels: int, *, declared_samples: int | None = None
) -> AudioPayload:
    """Decode the capture PCM payload (u16 declared samples, u16 pad, PCM f32le).

    When ``declared_samples`` is given (bare PCM without the prefix) the whole
    body is treated as PCM.  Short payloads are kept up to the last whole
    frame so a damaged packet still contributes the audio it did carry.
    """
    if declared_samples is None:
        if len(body) < 4:
            raise ValueError("audio payload shorter than its 4-byte prefix")
        declared_samples = struct.unpack_from("!H", body, 0)[0]
        body = body[4:]
    if channels <= 0:
        raise ValueError("channels must be positive")
    frame_bytes = 4 * channels
    actual = len(body) // frame_bytes
    truncated = actual < declared_samples
    usable = min(actual, declared_samples) if declared_samples else actual
    raw = body[: usable * frame_bytes]
    samples = np.frombuffer(raw, dtype="<f4").astype(np.float32)
    samples = samples.reshape(usable, channels).copy()
    return AudioPayload(
        declared_samples=declared_samples,
        samples=samples,
        truncated=truncated,
        actual_samples=usable,
    )


def pcm_to_bytes(samples: np.ndarray) -> bytes:
    """Encode PCM with the capture prefix (inverse of decode_audio_payload)."""
    arr = np.ascontiguousarray(samples, dtype="<f4")
    prefix = struct.pack("!HH", min(arr.shape[0], 0xFFFF), 0)
    return prefix + arr.tobytes()


def encode_audio_payload(samples: np.ndarray) -> bytes:
    return pcm_to_bytes(samples)


# ---------------------------------------------------------------------------
# Sequence bookkeeping
# ---------------------------------------------------------------------------

SEQ_MODULUS = 1 << 16
SEQ_HALF = SEQ_MODULUS // 2


def seq_delta(previous: int, current: int) -> int:
    """Signed distance from ``previous`` to ``current`` on the 16-bit ring."""
    delta = (current - previous) % SEQ_MODULUS
    if delta > SEQ_HALF:
        delta -= SEQ_MODULUS
    return delta


class SequenceTracker:
    """Track RTP sequence numbers without being fooled by 16-bit wrap.

    The highest sequence seen defines the reference.  A sequence at or below
    ``highest - window`` is treated as a *late* packet (reordered), one already
    seen is a duplicate, and gaps left above the contiguous front become lost
    packets once they are older than the reorder window or the stream ends.
    """

    def __init__(self, window: int = 64) -> None:
        if window < 1:
            raise ValueError("window must be at least 1")
        self.window = window
        self.highest: int | None = None
        self._seen: set[int] = set()
        self._losses: list[int] = []
        self.duplicates = 0
        self.reordered = 0
        self.wraps = 0
        self.last_wrap_unwrapped: int | None = None
        self._last_accepted: int | None = None

    def _unwrap(self, sequence: int) -> int:
        if self.highest is None:
            return sequence
        base = (self.highest // SEQ_MODULUS) * SEQ_MODULUS
        candidate = base + sequence
        while candidate <= self.highest - SEQ_HALF:
            candidate += SEQ_MODULUS
        while candidate > self.highest + SEQ_HALF:
            candidate -= SEQ_MODULUS
        return candidate

    def observe(self, sequence: int) -> tuple[bool, bool, int]:
        """Return ``(accepted, reordered, unwrapped_sequence)``."""
        sequence &= 0xFFFF
        unwrapped = self._unwrap(sequence)
        if self.highest is None:
            self.highest = unwrapped
            self._seen.add(unwrapped)
            self._last_accepted = unwrapped
            return True, False, unwrapped
        if unwrapped in self._seen:
            self.duplicates += 1
            return False, False, unwrapped
        if unwrapped < self.highest:
            self.reordered += 1
            self._seen.add(unwrapped)
            self._losses = [x for x in self._losses if x != unwrapped]
            return True, True, unwrapped
        # Advance the reference and record any new gap.
        previous = self.highest
        gap_start = self.highest + 1
        for missing in range(gap_start, unwrapped):
            self._losses.append(missing)
        self._seen.add(unwrapped)
        self.highest = unwrapped
        self._last_accepted = unwrapped
        if (previous % SEQ_MODULUS) >= (SEQ_MODULUS - SEQ_HALF) and (
            unwrapped % SEQ_MODULUS
        ) < SEQ_HALF:
            # The 16-bit ring numbers came around between these two packets.
            self.wraps += 1
            self.last_wrap_unwrapped = unwrapped
        return True, False, unwrapped

    def flush_lost(self) -> list[int]:
        """Declare all still-missing sequences (modulus-relative) as lost."""
        lost = list(self._losses)
        self._losses.clear()
        return [x % SEQ_MODULUS for x in lost]

    @property
    def pending_losses(self) -> int:
        return len(self._losses)

    def relative(self, unwrapped: int) -> int:
        """Map an unwrapped sequence back onto the 16-bit ring."""
        return unwrapped % SEQ_MODULUS


# ---------------------------------------------------------------------------
# Video metadata records
# ---------------------------------------------------------------------------


@dataclass
class VideoRecord:
    """One video record: timing plus colour metadata emitted by the encoder."""

    index: int
    pts_samples: int
    duration_samples: int
    metadata: dict[str, Any] = field(default_factory=dict)
    malformed: bool = False

    def time_range(self, clock_rate: int) -> tuple[float, float]:
        return (
            self.pts_samples / clock_rate,
            (self.pts_samples + self.duration_samples) / clock_rate,
        )

    def to_json(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "pts_samples": self.pts_samples,
            "duration_samples": self.duration_samples,
            "metadata": dict(self.metadata),
        }


def decode_video_record(payload: bytes, index: int) -> VideoRecord:
    """Decode a video metadata record; never raises on malformed JSON."""
    try:
        data = json.loads(payload.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return VideoRecord(index=index, pts_samples=0, duration_samples=0,
                           malformed=True)
    if not isinstance(data, dict):
        return VideoRecord(index=index, pts_samples=0, duration_samples=0,
                           malformed=True)
    try:
        pts = int(data.get("pts_samples", 0))
        duration = int(data.get("duration_samples", 0))
    except (TypeError, ValueError):
        return VideoRecord(index=index, pts_samples=0, duration_samples=0,
                           malformed=True)
    meta = data.get("metadata", {})
    if not isinstance(meta, dict):
        meta = {}
    return VideoRecord(
        index=index,
        pts_samples=pts,
        duration_samples=max(duration, 0),
        metadata=meta,
    )


def encode_video_record(
    pts_samples: int, duration_samples: int, metadata: dict[str, Any]
) -> bytes:
    body = {
        "pts_samples": int(pts_samples),
        "duration_samples": int(duration_samples),
        "metadata": dict(metadata),
    }
    return json.dumps(body, sort_keys=True).encode("utf-8")


__all__ = [
    "AudioPayload",
    "CaptureFormatError",
    "CaptureRecord",
    "HEADER_SIZE",
    "KIND_RTCP",
    "KIND_RTP",
    "KIND_VIDEO",
    "MAGIC",
    "RtpHeader",
    "SEQ_MODULUS",
    "SequenceTracker",
    "StreamSpec",
    "VideoRecord",
    "decode_audio_payload",
    "decode_video_record",
    "encode_audio_payload",
    "encode_video_record",
    "parse_rtp_header",
    "read_capture",
    "seq_delta",
    "write_record",
    "write_truncated_record",
]
