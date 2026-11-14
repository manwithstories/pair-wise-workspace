"""Plain MP4 export with moov before mdat and a rebuilt sample table."""

from __future__ import annotations

import os
import struct
import tempfile

from mkvtomp4.mp4.boxes import box, u32, u64
from mkvtomp4.mp4.moov import build_moov
from mkvtomp4.timing.normalize import NormalizedMedia, NormalizedTrack


class ExportError(RuntimeError):
    pass


def _ftyp() -> bytes:
    payload = b"isom" + u32(0x200) + b"isomiso6mp41"
    return box("ftyp", payload)


def _layout(media: NormalizedMedia) -> tuple[list[tuple[NormalizedTrack, int]], int]:
    entries: list[tuple[NormalizedTrack, int]] = []
    total = 0
    for track in media.tracks:
        for index, size in enumerate(track.sizes):
            entries.append((track, index))
            total += int(size)
    return entries, total


def _offset_map(
    media: NormalizedMedia,
    data_start: int,
) -> tuple[dict[int, list[int]], int]:
    offsets: dict[int, list[int]] = {track.track.mkv_track: [] for track in media.tracks}
    cursor = data_start
    for track in media.tracks:
        for size in track.sizes:
            offsets[track.track.mkv_track].append(cursor)
            cursor += int(size)
    return offsets, cursor


def _write_frame(source, output, offset: int, size: int) -> None:
    source.seek(offset)
    remaining = size
    while remaining:
        chunk = source.read(min(remaining, 1 << 20))
        if not chunk:
            raise ExportError(f"source ended while reading sample at {offset}")
        output.write(chunk)
        remaining -= len(chunk)


def export_plain(media: NormalizedMedia, out_path: str) -> dict[str, object]:
    os.makedirs(os.path.dirname(os.path.abspath(out_path)), exist_ok=True)
    ftyp = _ftyp()
    entries, payload_size = _layout(media)
    placeholder = {track.track.mkv_track: [0] * track.sample_count for track in media.tracks}
    moov_stub = build_moov(media, placeholder, fragmented=False)
    mdat_header_size = 8 if 8 + payload_size <= 0xFFFFFFFF else 16
    data_start = len(ftyp) + len(moov_stub) + mdat_header_size
    offsets, data_end = _offset_map(media, data_start)
    moov = build_moov(media, offsets, fragmented=False)
    if len(moov) != len(moov_stub):
        raise ExportError("moov size changed after chunk-offset patch")
    total_size = len(ftyp) + len(moov) + mdat_header_size + payload_size
    if total_size > 0xFFFFFFFF:
        raise ExportError("plain MP4 exceeds 32-bit box size support")
    descriptor, temp_name = tempfile.mkstemp(
        prefix=".mkvtomp4-", suffix=".mp4", dir=os.path.dirname(os.path.abspath(out_path))
    )
    try:
        with os.fdopen(descriptor, "wb") as output, open(media.source.path, "rb") as source:
            output.write(ftyp)
            output.write(moov)
            output.write(struct.pack(">I4s", 8 + payload_size, b"mdat"))
            for track, index in entries:
                _write_frame(
                    source,
                    output,
                    int(track.source_offsets[index]),
                    int(track.sizes[index]),
                )
            output.flush()
            os.fsync(output.fileno())
        os.replace(temp_name, out_path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise
    return {
        "path": out_path,
        "layout": "plain",
        "moov_before_mdat": True,
        "mdat_data_start": data_start,
        "mdat_data_end": data_end,
        "size": total_size,
    }
