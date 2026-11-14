"""Parse sample tables and fragment runs back out of an MP4 file."""

from __future__ import annotations

import struct
from dataclasses import dataclass

from mkvtomp4.mp4.boxes import BoxError, BoxHeader, child_headers, iter_box_headers, read_payload


class ReadbackError(ValueError):
    """Raised when the output cannot be parsed as the expected MP4 shape."""


@dataclass
class OutputTrack:
    track_id: int
    timescale: int
    sample_count: int
    first_pts: int
    last_pts: int
    first_dts: int
    last_dts: int
    keyframe_count: int


def _children(payload: bytes) -> list[BoxHeader]:
    return child_headers(payload)


def _find(payload: bytes, box_type: str) -> BoxHeader | None:
    return next((header for header in _children(payload) if header.box_type == box_type), None)


def _sub(payload: bytes, header: BoxHeader | None) -> bytes:
    if header is None:
        raise ReadbackError("required MP4 box is missing")
    return payload[header.data_offset : header.end]


def _descend(payload: bytes, *types: str) -> bytes:
    current = payload
    for box_type in types:
        current = _sub(current, _find(current, box_type))
    return current


def _u32(data: bytes, offset: int) -> int:
    return struct.unpack(">I", data[offset : offset + 4])[0]


def _i32(data: bytes, offset: int) -> int:
    return struct.unpack(">i", data[offset : offset + 4])[0]


def _elst_start_offset(trak_payload: bytes) -> int:
    edts = _find(trak_payload, "edts")
    if edts is None:
        return 0
    elst = _sub(_sub(trak_payload, edts), _find(_sub(trak_payload, edts), "elst"))
    version = elst[0]
    count = _u32(elst, 4)
    offset = 8
    start = 0
    for _ in range(count):
        if version == 1:
            segment_duration = struct.unpack(">Q", elst[offset : offset + 8])[0]
            media_time = struct.unpack(">q", elst[offset + 8 : offset + 16])[0]
            offset += 20
        else:
            segment_duration = _u32(elst, offset)
            media_time = _i32(elst, offset + 4)
            offset += 12
        if media_time == -1:
            start += segment_duration
    return start


def _ctts_deltas(ctts: bytes | None, sample_count: int) -> list[int]:
    if ctts is None:
        return [0] * sample_count
    version = ctts[0]
    count = _u32(ctts, 4)
    deltas: list[int] = []
    offset = 8
    for _ in range(count):
        run = _u32(ctts, offset)
        delta = _i32(ctts, offset + 4) if version == 1 else _u32(ctts, offset + 4)
        deltas.extend([delta] * run)
        offset += 8
    if len(deltas) != sample_count:
        raise ReadbackError("ctts entry count does not match sample count")
    return deltas


def _parse_plain_trak(trak_payload: bytes, movie_timescale: int) -> OutputTrack:
    tkhd = _sub(trak_payload, _find(trak_payload, "tkhd"))
    mdhd = _descend(trak_payload, "mdia", "mdhd")
    stbl = _descend(trak_payload, "mdia", "minf", "stbl")
    track_id = _u32(tkhd, 12)
    timescale = _u32(mdhd, 12)
    stsz = _sub(stbl, _find(stbl, "stsz"))
    sample_count = _u32(stsz, 8)
    stts = _sub(stbl, _find(stbl, "stts"))
    entry_count = _u32(stts, 4)
    durations: list[int] = []
    offset = 8
    for _ in range(entry_count):
        count = _u32(stts, offset)
        durations.extend([_u32(stts, offset + 4)] * count)
        offset += 8
    if len(durations) != sample_count:
        raise ReadbackError("stts entry count does not match sample count")
    ctts = _find(stbl, "ctts")
    deltas = _ctts_deltas(_sub(stbl, ctts) if ctts else None, sample_count)
    stss = _find(stbl, "stss")
    keyframe_count = sample_count if stss is None else _u32(_sub(stbl, stss), 4)
    start = int(round(_elst_start_offset(trak_payload) * timescale / movie_timescale))
    dts_values = [start]
    for duration in durations[:-1]:
        dts_values.append(dts_values[-1] + duration)
    pts_values = [dts + delta for dts, delta in zip(dts_values, deltas)]
    return OutputTrack(
        track_id,
        timescale,
        sample_count,
        pts_values[0] if pts_values else start,
        pts_values[-1] if pts_values else start,
        dts_values[0] if dts_values else start,
        dts_values[-1] if dts_values else start,
        keyframe_count,
    )


def _track_timescales(moov_payload: bytes) -> dict[int, int]:
    result: dict[int, int] = {}
    for trak in _children(moov_payload):
        if trak.box_type != "trak":
            continue
        trak_payload = _sub(moov_payload, trak)
        tkhd = _sub(trak_payload, _find(trak_payload, "tkhd"))
        mdhd = _descend(trak_payload, "mdia", "mdhd")
        result[_u32(tkhd, 12)] = _u32(mdhd, 12)
    return result


def _parse_trun(trun: bytes) -> list[tuple[int, int, bool, int]]:
    flags = int.from_bytes(trun[1:4], "big")
    count = _u32(trun, 4)
    offset = 8
    if flags & 0x000001:
        offset += 4
    first_flags: int | None = None
    if flags & 0x000004:
        first_flags = _u32(trun, offset)
        offset += 4
    samples: list[tuple[int, int, bool, int]] = []
    for index in range(count):
        duration = _u32(trun, offset) if flags & 0x000100 else 0
        if flags & 0x000100:
            offset += 4
        size = _u32(trun, offset) if flags & 0x000200 else 0
        if flags & 0x000200:
            offset += 4
        if flags & 0x000400:
            sample_flags = _u32(trun, offset)
            offset += 4
        elif index == 0 and first_flags is not None:
            sample_flags = first_flags
        else:
            sample_flags = 0
        cto = _i32(trun, offset) if flags & 0x000800 else 0
        if flags & 0x000800:
            offset += 4
        samples.append((duration, size, not bool(sample_flags & 0x00010000), cto))
    return samples


def _parse_fragment_tracks(path: str) -> tuple[dict[int, OutputTrack], int]:
    aggregates: dict[int, dict[str, list[int]]] = {}
    moof_count = 0
    with open(path, "rb") as stream:
        headers = list(iter_box_headers(stream))
        moov = next((header for header in headers if header.box_type == "moov"), None)
        if moov is None:
            raise ReadbackError("output has no moov")
        timescales = _track_timescales(read_payload(stream, moov))
        for header in headers:
            if header.box_type != "moof":
                continue
            moof_count += 1
            moof_payload = read_payload(stream, header)
            for traf in _children(moof_payload):
                if traf.box_type != "traf":
                    continue
                traf_payload = _sub(moof_payload, traf)
                tfhd = _sub(traf_payload, _find(traf_payload, "tfhd"))
                track_id = _u32(tfhd, 4)
                tfdt = _sub(traf_payload, _find(traf_payload, "tfdt"))
                base = struct.unpack(">Q", tfdt[4:12])[0] if tfdt[0] == 1 else _u32(tfdt, 4)
                trun = _sub(traf_payload, _find(traf_payload, "trun"))
                bucket = aggregates.setdefault(track_id, {"dts": [], "pts": [], "sizes": [], "keys": []})
                current = base
                for duration, size, sync, cto in _parse_trun(trun):
                    bucket["dts"].append(current)
                    bucket["pts"].append(current + cto)
                    bucket["sizes"].append(size)
                    bucket["keys"].append(sync)
                    current += duration
    tracks: dict[int, OutputTrack] = {}
    for track_id, bucket in aggregates.items():
        dts, pts = bucket["dts"], bucket["pts"]
        tracks[track_id] = OutputTrack(
            track_id,
            timescales.get(track_id, 0),
            len(bucket["sizes"]),
            pts[0] if pts else 0,
            pts[-1] if pts else 0,
            dts[0] if dts else 0,
            dts[-1] if dts else 0,
            sum(bucket["keys"]),
        )
    return tracks, moof_count


def _parse_plain_tracks(path: str) -> dict[int, OutputTrack]:
    with open(path, "rb") as stream:
        headers = list(iter_box_headers(stream))
        moov = next((header for header in headers if header.box_type == "moov"), None)
        if moov is None:
            raise ReadbackError("output has no moov")
        moov_payload = read_payload(stream, moov)
        mvhd = _sub(moov_payload, _find(moov_payload, "mvhd"))
        movie_timescale = _u32(mvhd, 12)
        result: dict[int, OutputTrack] = {}
        for trak in _children(moov_payload):
            if trak.box_type == "trak":
                parsed = _parse_plain_trak(_sub(moov_payload, trak), movie_timescale)
                result[parsed.track_id] = parsed
        return result


def read_output_tracks(path: str) -> tuple[dict[int, OutputTrack], bool, int]:
    with open(path, "rb") as stream:
        top = [header.box_type for header in iter_box_headers(stream)]
    fragmented = "moof" in top
    if fragmented:
        tracks, fragment_count = _parse_fragment_tracks(path)
        return tracks, True, fragment_count
    return _parse_plain_tracks(path), False, 0
