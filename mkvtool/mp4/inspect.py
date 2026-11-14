"""Read enough of an MP4 to verify the locally generated sample tables."""

from __future__ import annotations

from mkvtool.errors import InputParseError
from mkvtool.mp4.boxes import Mp4Box, find, parse_boxes, require


def _body(box: Mp4Box) -> bytes:
    return box.payload[4:]


def _read_stts(body: bytes) -> list[int]:
    entries = int.from_bytes(body[:4], "big")
    durations: list[int] = []
    cursor = 4
    for _ in range(entries):
        count = int.from_bytes(body[cursor : cursor + 4], "big")
        delta = int.from_bytes(body[cursor + 4 : cursor + 8], "big")
        durations.extend([delta] * count)
        cursor += 8
    return durations


def _read_ctts(body: bytes) -> list[int]:
    entries = int.from_bytes(body[:4], "big")
    offsets: list[int] = []
    cursor = 4
    for _ in range(entries):
        count = int.from_bytes(body[cursor : cursor + 4], "big")
        offset = int.from_bytes(body[cursor + 4 : cursor + 8], "big", signed=True)
        offsets.extend([offset] * count)
        cursor += 8
    return offsets


def _read_stss(body: bytes) -> list[int]:
    entries = int.from_bytes(body[:4], "big")
    return [int.from_bytes(body[4 + index * 4 : 8 + index * 4], "big") for index in range(entries)]


def _read_stco(body: bytes) -> list[int]:
    entries = int.from_bytes(body[:4], "big")
    return [int.from_bytes(body[4 + index * 4 : 8 + index * 4], "big") for index in range(entries)]


def _read_stsc(body: bytes) -> list[tuple[int, int]]:
    entries = int.from_bytes(body[:4], "big")
    values: list[tuple[int, int]] = []
    cursor = 4
    for _ in range(entries):
        first = int.from_bytes(body[cursor : cursor + 4], "big")
        count = int.from_bytes(body[cursor + 4 : cursor + 8], "big")
        values.append((first, count))
        cursor += 12
    return values


def _read_stsz(body: bytes) -> tuple[int, list[int]]:
    constant = int.from_bytes(body[:4], "big")
    count = int.from_bytes(body[4:8], "big")
    if constant:
        return count, [constant] * count
    sizes = [
        int.from_bytes(body[8 + index * 4 : 12 + index * 4], "big")
        for index in range(count)
    ]
    return count, sizes


def _chunk_sample_count(stsc: list[tuple[int, int]], chunks: int) -> int:
    if not stsc:
        return 0
    total = 0
    for index, (first, count) in enumerate(stsc):
        last = stsc[index + 1][0] - 1 if index + 1 < len(stsc) else chunks
        total += max(0, last - first + 1) * count
    return total


def inspect_mp4_bytes(data: bytes) -> dict:
    roots = parse_boxes(data)
    moov = require(roots, b"moov")
    mdat_box = find(roots, b"mdat")
    moov_children = moov.children()
    mvhd = require(moov_children, b"mvhd")
    movie_timescale = int.from_bytes(_body(mvhd)[8:12], "big")
    tracks: list[dict] = []
    for trak in [item for item in moov_children if item.type == b"trak"]:
        trak_children = trak.children()
        tkhd = require(trak_children, b"tkhd")
        track_id = int.from_bytes(_body(tkhd)[8:12], "big")
        mdia = require(trak_children, b"mdia").children()
        mdhd = require(mdia, b"mdhd")
        mdhd_body = _body(mdhd)
        timescale = int.from_bytes(mdhd_body[8:12], "big")
        hdlr = require(mdia, b"hdlr")
        handler = _body(hdlr)[4:8].decode("ascii", errors="replace")
        minf = require(mdia, b"minf").children()
        stbl = require(minf, b"stbl").children()
        stsd = require(stbl, b"stsd")
        entry_type = stsd.payload[12:16].decode("ascii", errors="replace")
        stts = require(stbl, b"stts")
        durations = _read_stts(_body(stts))
        ctts_box = find(stbl, b"ctts")
        offsets = _read_ctts(_body(ctts_box)) if ctts_box else [0] * len(durations)
        stss_box = find(stbl, b"stss")
        sync = _read_stss(_body(stss_box)) if stss_box else list(range(1, len(durations) + 1))
        stco = _read_stco(_body(require(stbl, b"stco")))
        stsc = _read_stsc(_body(require(stbl, b"stsc")))
        sample_count, sizes = _read_stsz(_body(require(stbl, b"stsz")))
        if len(durations) != sample_count or len(offsets) != sample_count:
            raise InputParseError(f"track {track_id} sample table counts disagree")
        if any(index <= 0 or index > sample_count for index in sync):
            raise InputParseError(f"track {track_id} stss contains an invalid sample number")
        if _chunk_sample_count(stsc, len(stco)) != sample_count:
            raise InputParseError(f"track {track_id} stsc does not cover all chunks")
        dts: list[int] = []
        current = 0
        for duration in durations:
            dts.append(current)
            current += duration
        pts = [dts_value + offset for dts_value, offset in zip(dts, offsets)]
        mdat_start = mdat_box.offset + mdat_box.header_size if mdat_box else None
        mdat_end = mdat_box.offset + mdat_box.size if mdat_box else None
        chunks_valid = bool(stco) and mdat_start is not None
        if chunks_valid:
            chunks_valid = all(mdat_start <= offset < mdat_end for offset in stco)
        tracks.append({
            "track_id": track_id,
            "handler": handler,
            "codec": entry_type,
            "timescale": timescale,
            "frame_count": sample_count,
            "first_dts_ms": dts[0] / timescale * 1000 if dts else None,
            "first_pts_ms": min(pts) / timescale * 1000 if pts else None,
            "last_pts_ms": max(pts) / timescale * 1000 if pts else None,
            "keyframes": len(sync),
            "dts_monotonic": all(a < b for a, b in zip(dts, dts[1:])) if len(dts) > 1 else True,
            "chunk_count": len(stco),
            "chunk_offsets_valid": chunks_valid,
            "sample_sizes": sizes,
        })
    return {
        "movie_timescale": movie_timescale,
        "has_ftyp": find(roots, b"ftyp") is not None,
        "has_mdat": mdat_box is not None,
        "moov_offset": moov.offset,
        "tracks": tracks,
    }


def inspect_mp4_file(path: str) -> dict:
    with open(path, "rb") as handle:
        return inspect_mp4_bytes(handle.read())
