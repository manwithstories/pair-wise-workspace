"""Self-describing frame-sequence format: writer and streaming parser.

On-disk layout, all integers big-endian::

    MAGIC(4) | file_header(31) | frame*
    frame = frame_header(62) | payload(payload_len)

The parser streams one file at a time, verifies payload SHA-256 digests and
lengths, and reports every failure with the file name, frame sequence number
and the *raw byte offset* at which the failure starts.
"""

from __future__ import annotations

import hashlib
import struct
from pathlib import Path

from .model import (
    ALL_COLOR_PRIMARIES,
    ALL_TRANSFERS,
    DIGEST_SIZE,
    FILE_HEADER_FORMAT,
    FILE_HEADER_SIZE,
    FILE_MAGIC_SIZE,
    FRAME_HEADER_FORMAT,
    FRAME_HEADER_SIZE,
    MAX_FILE_BYTES,
    MAX_FRAMES_PER_TRACK,
    MAX_HEIGHT,
    MAX_PAYLOAD_BYTES,
    MAX_WIDTH,
    MAGIC,
    ChecksumError,
    FormatError,
    FrameHeader,
    FrameRecord,
    MediaMixError,
    MetadataError,
    TrackHeader,
    TrackParseResult,
)

_VALID_BITS = frozenset(range(1, 65))


def make_frame_header(
    seq: int,
    clock: int,
    payload: bytes,
    *,
    fps_num: int,
    fps_den: int,
    width: int,
    height: int,
    color_primaries: str = "709",
    transfer: str = "1886",
) -> FrameHeader:
    """Build a header for ``payload``, computing its SHA-256 digest."""
    return FrameHeader(
        seq=seq,
        clock=clock,
        fps_num=fps_num,
        fps_den=fps_den,
        width=width,
        height=height,
        color_primaries=color_primaries,
        transfer=transfer,
        payload_len=len(payload),
        digest=hashlib.sha256(payload).digest(),
    )


def pack_file_header(header: TrackHeader) -> bytes:
    tid = header.track_id.encode("ascii")[:16]
    return struct.pack(
        FILE_HEADER_FORMAT,
        MAGIC,
        tid.ljust(16, b"\x00"),
        header.clock_rate,
        header.clock_bits,
        header.seq_bits,
        0,  # reserved
    )


def pack_frame_header(header: FrameHeader) -> bytes:
    return struct.pack(
        FRAME_HEADER_FORMAT,
        header.seq,
        header.clock,
        header.fps_num,
        header.fps_den,
        header.width,
        header.height,
        # fixed-width tag fields keep the header size stable
        header.color_primaries.encode("ascii")[:4].ljust(4, b"\x00"),
        header.transfer.encode("ascii")[:4].ljust(4, b"\x00"),
        header.payload_len,
        header.digest,
    )


def write_track_file(
    path: str | Path,
    header: TrackHeader,
    frames: list[tuple[FrameHeader, bytes]],
) -> str:
    """Write a complete track file and return its whole-file SHA-256."""
    _validate_file_header(header)
    if len(frames) > MAX_FRAMES_PER_TRACK:
        raise MetadataError(
            f"frame count {len(frames)} exceeds limit {MAX_FRAMES_PER_TRACK}",
            path=os_fspath(path),
        )
    hasher = hashlib.sha256()
    tmp = Path(path).with_suffix(Path(path).suffix + ".tmp")
    with tmp.open("wb") as fh:
        chunk = pack_file_header(header)
        fh.write(chunk)
        hasher.update(chunk)
        for hdr, payload in frames:
            _validate_frame_header(hdr, -1, os_fspath(path))
            if len(payload) != hdr.payload_len:
                raise MetadataError(
                    "payload length does not match header",
                    path=os_fspath(path),
                    frame_seq=hdr.seq,
                )
            chunk = pack_frame_header(hdr) + payload
            fh.write(chunk)
            hasher.update(chunk)
    tmp.replace(path)
    return hasher.hexdigest()


def os_fspath(path: str | Path) -> str:
    return path if isinstance(path, str) else str(path)


def _validate_file_header(header: TrackHeader) -> None:
    if not header.track_id or len(header.track_id.encode("ascii")) > 16:
        raise MetadataError(
            f"track_id must be 1..16 ASCII bytes, got {header.track_id!r}"
        )
    if header.clock_rate <= 0:
        raise MetadataError(f"clock_rate must be positive, got {header.clock_rate}")
    if header.clock_bits not in _VALID_BITS or header.seq_bits not in _VALID_BITS:
        raise MetadataError(
            f"counter widths must be in 1..64, got clock_bits="
            f"{header.clock_bits} seq_bits={header.seq_bits}"
        )


def _decode_tag(raw: bytes) -> str:
    return raw.rstrip(b"\x00").decode("ascii", errors="replace")


def _validate_frame_header(hdr: FrameHeader, offset: int, path: str) -> None:
    """Raise :class:`MetadataError` for any illegal self-described metadata."""
    def err(msg: str, seq: int | None = hdr.seq) -> None:
        raise MetadataError(msg, path=path, frame_seq=seq, offset=offset)

    if hdr.fps_num <= 0 or hdr.fps_den <= 0:
        err(f"frame rate {hdr.fps_num}/{hdr.fps_den} must have positive terms")
    if not (1 <= hdr.width <= MAX_WIDTH and 1 <= hdr.height <= MAX_HEIGHT):
        err(f"frame dimensions {hdr.width}x{hdr.height} outside 1..{MAX_WIDTH}/{MAX_HEIGHT}")
    if hdr.payload_len <= 0 or hdr.payload_len > MAX_PAYLOAD_BYTES:
        err(f"payload_len {hdr.payload_len} outside 1..{MAX_PAYLOAD_BYTES}")
    if hdr.payload_len != hdr.width * hdr.height * 3:
        err(
            f"payload_len {hdr.payload_len} != RGB byte count "
            f"{hdr.width * hdr.height * 3}"
        )
    if len(hdr.digest) != DIGEST_SIZE:
        err("digest field is not 32 bytes")
    if hdr.color_primaries not in ALL_COLOR_PRIMARIES:
        err(f"unknown color primaries {hdr.color_primaries!r}")
    if hdr.transfer not in ALL_TRANSFERS:
        err(f"unknown transfer characteristic {hdr.transfer!r}")
    if hdr.seq < 0 or hdr.clock < 0:
        err("negative sequence or clock value")


def _read_exact(fh, size: int, offset: int, path: str, what: str) -> bytes:
    """Read exactly ``size`` bytes; a short read is a truncation error."""
    data = fh.read(size)
    if len(data) != size:
        raise FormatError(
            f"truncated {what}: wanted {size} bytes, got {len(data)}",
            path=path,
            offset=offset,
        )
    return data


def parse_track_file(path: str | Path) -> TrackParseResult:
    """Stream-parse a track file, returning good frames and anchored errors.

    A fatal structural error stops parsing of this single track; callers may
    isolate it and continue with the remaining tracks.
    """
    path = os_fspath(path)
    result = TrackParseResult(header=None, path=path)  # type: ignore[arg-type]
    file_hasher = hashlib.sha256()
    try:
        with open(path, "rb") as fh:
            magic = fh.read(len(MAGIC))
            file_hasher.update(magic)
            if len(magic) < len(MAGIC):
                raise FormatError(
                    f"truncated magic: wanted {len(MAGIC)} bytes, got {len(magic)}",
                    path=path,
                    offset=0,
                )
            if magic != MAGIC:
                raise FormatError(f"bad magic {magic!r}, expected {MAGIC!r}", path=path, offset=0)

            header_blob = _read_exact(fh, FILE_HEADER_SIZE - FILE_MAGIC_SIZE, fh.tell(), path, "file header")
            file_hasher.update(header_blob)
            _, tid_raw, clock_rate, clock_bits, seq_bits, _reserved = struct.unpack(
                FILE_HEADER_FORMAT, MAGIC + header_blob
            )
            result.header = TrackHeader(
                track_id=_decode_tag(tid_raw),
                clock_rate=clock_rate,
                clock_bits=clock_bits,
                seq_bits=seq_bits,
            )
            _validate_file_header(result.header)

            while True:
                header_offset = fh.tell()
                blob = fh.read(FRAME_HEADER_SIZE)
                if not blob:
                    break  # clean end of file
                file_hasher.update(blob)
                if len(blob) < FRAME_HEADER_SIZE:
                    raise FormatError(
                        f"truncated frame header: got {len(blob)}/{FRAME_HEADER_SIZE} bytes",
                        path=path,
                        offset=header_offset,
                    )
                (
                    seq, clock, fps_num, fps_den, width, height,
                    prim_raw, xfer_raw, payload_len, digest,
                ) = struct.unpack(FRAME_HEADER_FORMAT, blob)
                hdr = FrameHeader(
                    seq=seq, clock=clock, fps_num=fps_num, fps_den=fps_den,
                    width=width, height=height,
                    color_primaries=_decode_tag(prim_raw),
                    transfer=_decode_tag(xfer_raw),
                    payload_len=payload_len, digest=digest,
                )
                # Metadata is validated before trusting payload_len.
                _validate_frame_header(hdr, header_offset, path)
                payload_offset = fh.tell()
                payload = _read_exact(fh, payload_len, payload_offset, path, "payload")
                file_hasher.update(payload)
                if hashlib.sha256(payload).digest() != digest:
                    raise ChecksumError(
                        "payload SHA-256 mismatch",
                        path=path, frame_seq=seq, offset=payload_offset,
                    )
                if len(result.frames) >= MAX_FRAMES_PER_TRACK:
                    raise MetadataError(
                        f"frame count exceeds limit {MAX_FRAMES_PER_TRACK}",
                        path=path, frame_seq=seq, offset=header_offset,
                    )
                result.frames.append(
                    FrameRecord(
                        header=hdr,
                        payload=payload,
                        header_offset=header_offset,
                        payload_offset=payload_offset,
                        file_seq=len(result.frames),
                    )
                )
                if fh.tell() > MAX_FILE_BYTES:
                    raise FormatError(
                        f"file exceeds {MAX_FILE_BYTES} bytes",
                        path=path, offset=fh.tell(),
                    )
    except MediaMixError as exc:
        result.errors.append(exc)
    except OSError as exc:
        result.errors.append(FormatError(f"unreadable file: {exc}", path=path))
    result.file_digest = file_hasher.hexdigest()
    return result
