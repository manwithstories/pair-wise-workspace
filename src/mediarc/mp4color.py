"""Container-level colour reader.

PyAV 13 surfaces colour only through the codec context, which for MP4/MOV is
populated from the bitstream VUI rather than the ``colr`` sample-entry box that
ffprobe reports.  That makes the ``colr`` box — the authoritative interchange
metadata in MOV/MP4 — invisible to the probe.  This module reads it directly.

Only the box path ``moov/trak/mdia/minf/stbl/stsd/<entry>/colr`` is walked, and
only the header regions of the file are read, so reading a colour tag never
loads media payloads.
"""

from __future__ import annotations

import struct
from dataclasses import dataclass
from pathlib import Path
from typing import BinaryIO, Iterator

#: ISO/IEC 14496-12 colour types we understand.
COLR_TYPES = (b"nclx", b"nclc", b"prof", b"rICC")


@dataclass(slots=True)
class ColrBox:
    colour_type: str
    primaries: int | None
    transfer: int | None
    matrix: int | None
    full_range: bool | None

    def fields(self) -> dict[str, int]:
        """Map to the same field names the probe uses for codec values."""
        out: dict[str, int] = {}
        if self.primaries is not None:
            out["primaries"] = self.primaries
        if self.transfer is not None:
            out["transfer"] = self.transfer
        if self.matrix is not None:
            out["space"] = self.matrix
        if self.full_range is not None:
            out["range"] = 2 if self.full_range else 1
        return out


def _read_box_header(handle: BinaryIO, limit: int) -> tuple[int, bytes, int] | None:
    """Return ``(box_size, kind, header_length)`` or ``None`` at end of range."""
    start = handle.tell()
    if start + 8 > limit:
        return None
    header = handle.read(8)
    if len(header) < 8:
        return None
    size, kind = struct.unpack(">I4s", header)
    header_len = 8
    if size == 1:
        extra = handle.read(8)
        if len(extra) < 8:
            return None
        size = struct.unpack(">Q", extra)[0]
        header_len = 16
    elif size == 0:
        size = limit - start
    if size < header_len:
        return None
    return size, kind, header_len


def _children(handle: BinaryIO, start: int, end: int) -> Iterator[tuple[bytes, int, int]]:
    """Yield ``(kind, body_start, box_end)`` for boxes in ``[start, end)``."""
    position = start
    while position < end:
        handle.seek(position)
        header = _read_box_header(handle, end)
        if header is None:
            return
        size, kind, header_len = header
        body_start = position + header_len
        box_end = position + size
        if box_end > end or box_end <= body_start:
            return
        yield kind, body_start, box_end
        position = box_end


def _find(handle: BinaryIO, start: int, end: int, kind: bytes):
    for child, body_start, box_end in _children(handle, start, end):
        if child == kind:
            return body_start, box_end
    return None


def _parse_colr(handle: BinaryIO, body_start: int, box_end: int) -> ColrBox | None:
    """Parse a ``colr`` box *body* (the four boxes whose header was consumed)."""
    handle.seek(body_start)
    raw = handle.read(min(box_end - body_start, 11))
    if len(raw) < 8:
        return None
    colour_type = raw[0:4]
    if colour_type not in COLR_TYPES:
        return None
    if colour_type in (b"nclx", b"nclc"):
        if len(raw) < 10:
            return None
        primaries, transfer, matrix = struct.unpack(">HHH", raw[4:10])
        full_range = None
        if colour_type == b"nclx" and len(raw) >= 11:
            full_range = bool(raw[10] & 0x80)
        return ColrBox(colour_type.decode("ascii"), primaries, transfer, matrix, full_range)
    # prof / rICC carry an ICC profile; no enumerated values to record.
    return ColrBox(colour_type.decode("ascii"), None, None, None, None)


def _walk_video_traks(
    handle: BinaryIO, moov_start: int, moov_end: int
) -> list[int | None]:
    """Return the ``colr`` box per video track, in stream order.

    A ``None`` entry means the track is video but carries no ``colr``; such a
    track is excluded from the ``moov`` walk result so callers can tell "no
    colour box" from "not a video track".
    """
    boxes: list[int | None] = []
    for kind, trak_start, trak_end in _children(handle, moov_start, moov_end):
        if kind != b"trak":
            continue
        mdia = _find(handle, trak_start, trak_end, b"mdia")
        if mdia is None:
            continue
        hdlr = _find(handle, *mdia, b"hdlr")
        if hdlr is None:
            continue
        handle.seek(hdlr[0])
        entry = handle.read(min(hdlr[1] - hdlr[0], 12))
        if len(entry) < 12 or entry[8:12] != b"vide":
            continue
        minf = _find(handle, *mdia, b"minf")
        stbl = _find(handle, *minf, b"stbl") if minf else None
        stsd = _find(handle, *stbl, b"stsd") if stbl else None
        if stsd is None:
            boxes.append(None)
            continue
        handle.seek(stsd[0])
        entry_count_raw = handle.read(8)
        if len(entry_count_raw) < 8:
            boxes.append(None)
            continue
        entry_count = struct.unpack(">I", entry_count_raw[4:8])[0]
        entry_start = handle.tell()
        entry_header = _read_box_header(handle, stsd[1]) if entry_count else None
        if entry_header is None:
            boxes.append(None)
            continue
        entry_size, _kind, entry_header_len = entry_header
        # Visual sample entries carry a 78-byte preamble before child boxes.
        child_start = entry_start + entry_header_len + 78
        child_end = entry_start + entry_size
        box = None
        for child, body_start, box_end in _children(handle, child_start, child_end):
            if child == b"colr":
                box = _parse_colr(handle, body_start, box_end)
                break
        boxes.append(box)
    return boxes


def read_mp4_colr(path: str) -> list[ColrBox | None]:
    """Return one ``ColrBox`` (or ``None``) per video track, in stream order."""
    try:
        size = Path(path).stat().st_size
        with open(path, "rb") as handle:
            moov = None
            position = 0
            while position < size:
                handle.seek(position)
                header = _read_box_header(handle, size)
                if header is None:
                    break
                box_size, kind, header_len = header
                if kind == b"moov":
                    moov = (position + header_len, position + box_size)
                    break
                position += box_size
            if moov is None:
                return []
            return _walk_video_traks(handle, *moov)
    except OSError:
        return []
    except struct.error:
        return []


def read_mp4_keyframe_index(path: str) -> list[list[int]]:
    """Per video track, the sample numbers the ``stss`` box declares as sync.

    An empty list means the track has no ``stss`` at all, which for a reordered
    codec means "every sample is a sync sample" and cannot be compared.
    Returns ``[]`` when the file has no ``moov`` or is not an MP4 at all.
    """
    result: list[list[int]] = []
    try:
        size = Path(path).stat().st_size
        with open(path, "rb") as handle:
            moov = None
            position = 0
            while position < size:
                handle.seek(position)
                header = _read_box_header(handle, size)
                if header is None:
                    break
                box_size, kind, header_len = header
                if kind == b"moov":
                    moov = (position + header_len, position + box_size)
                    break
                position += box_size
            if moov is None:
                return []
            for kind, trak_start, trak_end in _children(handle, *moov):
                if kind != b"trak":
                    continue
                mdia = _find(handle, trak_start, trak_end, b"mdia")
                if mdia is None:
                    continue
                hdlr = _find(handle, *mdia, b"hdlr")
                if hdlr is None:
                    continue
                handle.seek(hdlr[0])
                entry = handle.read(min(hdlr[1] - hdlr[0], 12))
                if len(entry) < 12 or entry[8:12] != b"vide":
                    continue
                minf = _find(handle, *mdia, b"minf")
                stbl = _find(handle, *minf, b"stbl") if minf else None
                stss = _find(handle, *stbl, b"stss") if stbl else None
                if stss is None:
                    result.append([])
                    continue
                handle.seek(stss[0])
                header = handle.read(8)
                if len(header) < 8:
                    result.append([])
                    continue
                count = struct.unpack(">I", header[4:8])[0]
                if count > 10_000_000 or 8 + 4 * count > stss[1] - stss[0]:
                    result.append([])
                    continue
                raw = handle.read(4 * count)
                result.append(list(struct.unpack(f">{count}I", raw)))
            return result
    except (OSError, struct.error):
        return []


def container_color(path: str, format_name: str) -> dict[int, dict[str, int]]:
    """Colour fields keyed by PyAV stream index.

    MKV stores these as stream tags, which the probe already reads.  MP4/MOV
    needs the ``colr`` box walk implemented here.
    """
    if format_name not in ("mp4", "mov"):
        return {}
    result: dict[int, dict[str, int]] = {}
    for index, box in enumerate(read_mp4_colr(path)):
        if box is None:
            continue
        fields = box.fields()
        if fields:
            result[index] = fields
    return result
