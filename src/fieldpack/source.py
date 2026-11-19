"""Streaming reader for FPR1/FPR2 frame-record files.

Records are parsed sequentially in acquisition order.  Unknown versions,
out-of-bounds fields and corrupt checksums are reported with their raw
byte offset and the scanner resynchronises on the next record magic, so
recoverable later records are still yielded.  The file is memory-mapped
read-only: no frame payload is ever copied into logs or held longer than
the caller needs it.
"""
from __future__ import annotations

import mmap
import os
import struct
import zlib
from pathlib import Path
from typing import Iterator

from .model import (
    ALL_PIX_FMTS,
    MAX_DIMENSION,
    MAX_FRAME_BYTES,
    MAX_META_BYTES,
    FieldPackError,
    FrameRecord,
    Stage,
    frame_nbytes,
)

MAGIC_PREFIX = b"FPR"
MAGICS = {b"FPR1": 1, b"FPR2": 2}

# magic(4) version(1) flags(1) device(2) seq(4) ts(8) pixfmt(1) rsvd(1)
# width(2) height(2) meta_len(2) payload_len(4) crc32(4)            = 36
_V1 = struct.Struct("<4sBBHIQBBHHHII")
# v2 appends exposure_ns(8, signed) iso(4)                          = 48
_V2 = struct.Struct("<4sBBHIQBBHHHIIqI")
_HEADERS = {1: _V1, 2: _V2}

FLAG_KEYFRAME = 0x01


def _next_magic(data: mmap.mmap, start: int, end: int) -> int:
    """Next offset of any FPRx magic at or after ``start``; -1 if none."""
    pos = data.find(MAGIC_PREFIX, start, end)
    while pos != -1:
        if pos + 4 <= end and data[pos + 3 : pos + 4].isdigit():
            return pos
        pos = data.find(MAGIC_PREFIX, pos + 1, end)
    return -1


def _err(code: str, msg: str, offset: int) -> FieldPackError:
    return FieldPackError(code=code, message=msg, stage=Stage.PARSE, offset=offset)


def iter_records(path: str | os.PathLike[str]) -> Iterator[FrameRecord | FieldPackError]:
    """Yield FrameRecord items; corrupt spots yield FieldPackError instead.

    A truncated header or truncated record body terminates the scan (the
    tail is unrecoverable); every other error resynchronises and continues.
    """
    path = Path(path)
    with path.open("rb") as fh, mmap.mmap(fh.fileno(), 0, access=mmap.ACCESS_READ) as data:
        n = len(data)
        pos = 0
        while pos < n:
            if data[pos : pos + 3] != MAGIC_PREFIX:
                nxt = _next_magic(data, pos + 1, n)
                yield _err("bad-magic", "record magic not found", pos)
                if nxt == -1:
                    return
                pos = nxt
                continue
            magic = bytes(data[pos : pos + 4])
            version = MAGICS.get(magic)
            if version is None:
                yield _err("unknown-version", f"unrecognised record version {magic!r}", pos)
                nxt = _next_magic(data, pos + 4, n)
                if nxt == -1:
                    return
                pos = nxt
                continue
            hdr = _HEADERS[version]
            if pos + hdr.size > n:
                yield _err("truncated-header", "incomplete record header at tail", pos)
                return
            fields = hdr.unpack(data[pos : pos + hdr.size])
            (
                _magic,
                _ver,
                flags,
                device_id,
                seq,
                raw_ts,
                pix_code,
                _rsvd,
                width,
                height,
                meta_len,
                payload_len,
                crc,
            ) = fields[:13]
            pixfmt = ALL_PIX_FMTS.get(pix_code)
            bad = None
            if pixfmt is None:
                bad = f"unknown pixel format code {pix_code}"
            elif not (0 < width <= MAX_DIMENSION and 0 < height <= MAX_DIMENSION):
                bad = f"dimensions {width}x{height} out of bounds"
            elif meta_len > MAX_META_BYTES:
                bad = f"metadata length {meta_len} exceeds limit"
            elif payload_len > MAX_FRAME_BYTES:
                bad = f"payload length {payload_len} exceeds limit"
            else:
                want = frame_nbytes(pixfmt, width, height)
                if want >= 0 and payload_len != want:
                    bad = f"payload length {payload_len} != expected {want} for {pixfmt} {width}x{height}"
            if bad is not None:
                yield _err("field-out-of-bounds", bad, pos)
                nxt = _next_magic(data, pos + 4, n)
                if nxt == -1:
                    return
                pos = nxt
                continue
            body_start = pos + hdr.size
            body_end = body_start + meta_len + payload_len
            if body_end > n:
                yield _err("truncated-record", "record body cut off at tail", pos)
                return
            meta = bytes(data[body_start : body_start + meta_len])
            payload = bytes(data[body_start + meta_len : body_end])
            if zlib.crc32(meta + payload) & 0xFFFFFFFF != crc:
                yield _err("checksum-mismatch", "CRC32 of metadata+payload mismatch", pos)
                nxt = _next_magic(data, pos + 4, n)
                if nxt == -1:
                    return
                pos = nxt
                continue
            exposure_ns = iso = None
            if version == 2:
                exposure_ns, iso = fields[13], fields[14]
            yield FrameRecord(
                offset=pos,
                version=version,
                device_id=device_id,
                seq=seq,
                raw_ts_ns=raw_ts,
                pixfmt=pixfmt,
                width=width,
                height=height,
                keyframe=bool(flags & FLAG_KEYFRAME),
                meta=meta,
                payload=payload,
                exposure_ns=exposure_ns,
                iso=iso,
            )
            pos = body_end


def probe_source(path: str | os.PathLike[str]) -> dict[int, "TrackInfo"]:
    """Quick scan collecting static track properties for mapping/mux setup."""
    from .model import TrackInfo

    tracks: dict[int, TrackInfo] = {}
    for item in iter_records(path):
        if isinstance(item, FieldPackError):
            continue
        info = tracks.get(item.device_id)
        if info is None:
            tracks[item.device_id] = TrackInfo(
                device_id=item.device_id,
                pixfmt=item.pixfmt,
                width=item.width,
                height=item.height,
                record_count=1,
            )
        else:
            info.record_count += 1
    return tracks
