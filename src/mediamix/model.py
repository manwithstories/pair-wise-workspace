"""Shared time base, record types, limits and offset-carrying errors.

Every other mediamix module imports its primitive definitions from here so the
pipeline has one canonical representation of time, frame headers, tracks,
diagnostics and failures.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path

# ---------------------------------------------------------------------------
# Binary self-describing frame sequence format
# ---------------------------------------------------------------------------
# File:   MAGIC(4) | file_header(31) | frame*
# Frame:  frame_header(60) | payload(payload_len bytes)
MAGIC = b"MMX1"
FILE_MAGIC_SIZE = 4
# Big-endian: magic(4s) track_id(16s) clock_rate(Q) clock_bits(B) seq_bits(B) reserved(B)
FILE_HEADER_FORMAT = ">4s16sQBBB"
FILE_HEADER_SIZE = 31
# Big-endian fixed header (60 bytes), see pack_frame_header.
# I seq | I clock | H fps_num | H fps_den | H width | H height |
# 4s primaries | 4s transfer | I payload_len | 32s SHA-256
FRAME_HEADER_FORMAT = ">I I H H H H 4s 4s I 32s"
FRAME_HEADER_SIZE = 60
DIGEST_SIZE = 32
# Four-character codes packed into the frame header tag fields.
ALL_COLOR_PRIMARIES = frozenset({"709", "2020", "smp", "unk"})
ALL_TRANSFERS = frozenset({"1886", "pqhl", "hlg", "srgb", "lin"})

# ---------------------------------------------------------------------------
# Unified integer time scale
# ---------------------------------------------------------------------------
TICKS_PER_SECOND = 1_000_000_000  # one tick = one nanosecond on the master clock

# ---------------------------------------------------------------------------
# Resource caps (memory / amplification protection)
# ---------------------------------------------------------------------------
MAX_WIDTH = 1920
MAX_HEIGHT = 1080
MAX_PAYLOAD_BYTES = MAX_WIDTH * MAX_HEIGHT * 3
MAX_FRAMES_PER_TRACK = 10_000
MAX_TIMELINE_FRAMES = 100_000
MAX_TRACKS = 64
MAX_FILE_BYTES = 8 * 1024 * 1024 * 1024
MAX_PATH_PARTS = 64

# ---------------------------------------------------------------------------
# Process exit codes (documented in the CLI help)
# ---------------------------------------------------------------------------
EXIT_OK = 0
EXIT_CORRUPT_INPUT = 2
EXIT_CHECKPOINT_CONFLICT = 3
EXIT_VALIDATION_FAILED = 4


class MediaMixError(Exception):
    """Base class for all pipeline errors.

    ``offset`` is the raw byte offset inside the offending source file (or
    ``-1`` when the failure is not anchored to a file position).
    """

    exit_code = EXIT_CORRUPT_INPUT

    def __init__(
        self,
        message: str,
        *,
        path: str | os.PathLike[str] | None = None,
        frame_seq: int | None = None,
        offset: int = -1,
    ) -> None:
        super().__init__(message)
        self.message = message
        self.path = os.fspath(path) if path is not None else None
        self.frame_seq = frame_seq
        self.offset = offset

    def describe(self) -> str:
        loc = []
        if self.path is not None:
            loc.append(f"file={self.path}")
        if self.frame_seq is not None:
            loc.append(f"frame_seq={self.frame_seq}")
        if self.offset >= 0:
            loc.append(f"byte_offset={self.offset}")
        tail = f" ({', '.join(loc)})" if loc else ""
        return f"{self.message}{tail}"


class FormatError(MediaMixError):
    """Truncated header/frame, bad magic or unparseable structure."""


class ChecksumError(MediaMixError):
    """Payload SHA-256 does not match the digest recorded in the header."""


class MetadataError(MediaMixError):
    """Illegal metadata: zero frame-rate denominator, bad color tags, size."""


class AmbiguousWrapError(MediaMixError):
    """Counter jump is consistent with more than one wrap — unresolvable."""


class CheckpointConflictError(MediaMixError):
    """Stored fingerprint/parameters no longer match the current inputs."""

    exit_code = EXIT_CHECKPOINT_CONFLICT


class ValidationError(MediaMixError):
    """Independent re-read of an exported artifact failed."""

    exit_code = EXIT_VALIDATION_FAILED


class PathEscapeError(MediaMixError):
    """A path resolved outside the permitted working directory."""


# ---------------------------------------------------------------------------
# Records
# ---------------------------------------------------------------------------
@dataclass(frozen=True)
class FrameHeader:
    """Parsed fixed-size frame header (62 bytes, big-endian)."""

    seq: int            # frame sequence number (raw counter value)
    clock: int          # acquisition clock counter (raw, may be wrapped)
    fps_num: int        # presentation frame rate numerator
    fps_den: int        # presentation frame rate denominator
    width: int
    height: int
    color_primaries: str
    transfer: str
    payload_len: int
    digest: bytes       # 32-byte SHA-256 of the payload

    def as_dict(self) -> dict:
        return {
            "seq": self.seq,
            "clock": self.clock,
            "fps_num": self.fps_num,
            "fps_den": self.fps_den,
            "width": self.width,
            "height": self.height,
            "color_primaries": self.color_primaries,
            "transfer": self.transfer,
            "payload_len": self.payload_len,
            "sha256": self.digest.hex(),
        }


@dataclass
class FrameRecord:
    """One parsed frame plus its raw position inside the source file."""

    header: FrameHeader
    payload: bytes
    header_offset: int          # byte offset where the frame header starts
    payload_offset: int         # byte offset where the payload starts
    file_seq: int               # zero-based ordinal of the frame in the file


@dataclass
class TrackHeader:
    """Parsed 31-byte per-file header describing one track."""

    track_id: str
    clock_rate: int             # acquisition clock ticks per second
    clock_bits: int             # counter width for the clock field
    seq_bits: int               # counter width for the sequence field


@dataclass
class TrackParseResult:
    """Outcome of streaming one track file: good records plus fatal errors."""

    header: TrackHeader
    frames: list[FrameRecord] = field(default_factory=list)
    errors: list[MediaMixError] = field(default_factory=list)
    file_digest: str = ""       # SHA-256 over the whole file as stored on disk
    path: str = ""

    @property
    def ok(self) -> bool:
        return not self.errors


@dataclass
class Diagnostic:
    """Traceable timeline event, always anchored to track / sequence / tick."""

    kind: str                   # backward_time | missing_frame | overlap | tolerance
    track_id: str
    frame_seq: int | None
    tick: int
    detail: str

    def as_dict(self) -> dict:
        return {
            "kind": self.kind,
            "track_id": self.track_id,
            "frame_seq": self.frame_seq,
            "tick": self.tick,
            "detail": self.detail,
        }


def safe_resolve(workdir: str | os.PathLike[str], *parts: str) -> Path:
    """Resolve ``workdir/parts`` and reject any escape from ``workdir``.

    Symlinks are resolved before the containment check so a link pointing out
    of the working directory is treated the same as a literal ``..`` traversal.
    """
    base = Path(workdir).resolve()
    if len(parts) > MAX_PATH_PARTS:
        raise PathEscapeError("too many path components", path=str(workdir))
    candidate = base.joinpath(*parts)
    resolved = candidate.resolve()
    try:
        resolved.relative_to(base)
    except ValueError as exc:
        raise PathEscapeError(
            f"path {candidate} escapes working directory {base}",
            path=os.fspath(candidate),
        ) from exc
    return resolved
