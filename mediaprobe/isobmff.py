from __future__ import annotations

import os
import struct
from dataclasses import dataclass, field
from typing import BinaryIO

from . import codecs
from .boxes import Box, find_box, parse_boxes, read_box_data
from .errors import ContainerError, MediaError


VIDEO_CODECS = {
    "avc1": "h264",
    "avc3": "h264",
    "hvc1": "hevc",
    "hev1": "hevc",
    "vp09": "vp9",
    "av01": "av1",
}
AUDIO_CODECS = {"mp4a": "aac", "ac-3": "ac3", "ec-3": "eac3", "Opus": "opus"}
HANDLER_NAMES = {
    "vide": "video",
    "soun": "audio",
    "tmcd": "timecode",
    "sbtl": "subtitle",
    "text": "subtitle",
    "meta": "metadata",
}
LANGUAGE_ALIASES = {"eng": "en", "zho": "zh", "chi": "zh", "jpn": "ja", "und": None}


@dataclass(slots=True)
class SampleRange:
    offset: int
    size: int
    duration: int


@dataclass
class TrackInfo:
    track_id: int = 0
    handler_type: str = ""
    handler: str = ""
    codec: str = ""
    duration_ms: int = 0
    timescale: int = 0
    language: str | None = None
    width: int | None = None
    height: int | None = None
    profile: str | None = None
    level: str | None = None
    sample_rate: int | None = None
    channels: int | None = None
    stsd_count: int = 0
    stsd_actual: int = 0
    sample_count: int = 0
    has_stbl_samples: bool = False
    invalid_chunk_offsets: int = 0
    fragment_error: str | None = None
    base_media_decode_time: int | None = None
    duration_source: str = "mdhd"
    duration_box_path: str = ""
    duration_box_offset: int = 0
    codec_box_path: str = ""
    codec_box_offset: int = 0
    stsd_box_path: str = ""
    stsd_box_offset: int = 0
    chunk_box_path: str = ""
    chunk_box_offset: int = 0
    fragment_box_path: str = ""
    fragment_box_offset: int = 0
    sample_ranges: list[SampleRange] = field(default_factory=list)


@dataclass
class IsoInfo:
    path: str
    file_size: int
    brand: str
    brands: list[str]
    format: str
    movie_timescale: int
    duration_ms: int
    fragmented: bool
    tracks: list[TrackInfo]
    moov_path: str = ""
    moov_offset: int = 0


def _u16(data: bytes, offset: int) -> int:
    return struct.unpack_from(">H", data, offset)[0]


def _u32(data: bytes, offset: int) -> int:
    return struct.unpack_from(">I", data, offset)[0]


def _i32(data: bytes, offset: int) -> int:
    return struct.unpack_from(">i", data, offset)[0]


def _u64(data: bytes, offset: int) -> int:
    return struct.unpack_from(">Q", data, offset)[0]


def _language(raw: int) -> str | None:
    if raw == 0:
        return None
    chars = [((raw >> shift) & 0x1F) + 0x60 for shift in (10, 5, 0)]
    if all(0x61 <= value <= 0x7A for value in chars):
        code = "".join(chr(value) for value in chars)
        return LANGUAGE_ALIASES.get(code, code)
    return None


def _payload(fh: BinaryIO, box: Box, limit: int = 8 * 1024 * 1024) -> bytes:
    return read_box_data(fh, box, limit=limit)


def _parse_mvhd(data: bytes) -> tuple[int, int]:
    if len(data) < 24:
        raise ContainerError("BOX_MALFORMED", "mvhd payload is truncated", offset=0)
    if data[0] == 1:
        if len(data) < 32:
            raise ContainerError("BOX_MALFORMED", "mvhd v1 payload is truncated", offset=0)
        return _u32(data, 20), _u64(data, 24)
    return _u32(data, 12), _u32(data, 16)


def _parse_tkhd(data: bytes) -> tuple[int, int | None, int | None]:
    if len(data) < 84:
        return 0, None, None
    version = data[0]
    if version == 1:
        track_id = _u32(data, 20)
        width = _u32(data, 88) >> 16 if len(data) >= 96 else None
        height = _u32(data, 92) >> 16 if len(data) >= 96 else None
    else:
        track_id = _u32(data, 12)
        width = _u32(data, 76) >> 16 if len(data) >= 84 else None
        height = _u32(data, 80) >> 16 if len(data) >= 84 else None
    return track_id, width or None, height or None


def _parse_mdhd(data: bytes) -> tuple[int, int, str | None]:
    if len(data) < 24:
        raise ContainerError("BOX_MALFORMED", "mdhd payload is truncated", offset=0)
    if data[0] == 1:
        if len(data) < 34:
            raise ContainerError("BOX_MALFORMED", "mdhd v1 payload is truncated", offset=0)
        return _u32(data, 20), _u64(data, 24), _language(_u16(data, 32))
    return _u32(data, 12), _u32(data, 16), _language(_u16(data, 20))


def _parse_hdlr(data: bytes) -> tuple[str, str]:
    if len(data) < 12:
        raise ContainerError("BOX_MALFORMED", "hdlr payload is truncated", offset=0)
    handler_type = data[8:12].decode("latin-1", "replace")
    name = data[24:].split(b"\x00", 1)[0].decode("utf-8", "replace") if len(data) > 24 else ""
    return handler_type, name


def _parse_elst(data: bytes, movie_timescale: int) -> tuple[int, str | None]:
    if len(data) < 8:
        raise ContainerError("BOX_MALFORMED", "elst payload is truncated", offset=0)
    version = data[0]
    count = _u32(data, 4)
    position = 8
    total = 0
    error = None
    entry_size = 20 if version == 1 else 12
    for _ in range(count):
        if position + entry_size > len(data):
            error = "elst entry table is shorter than declared"
            break
        segment_duration = _u64(data, position) if version == 1 else _u32(data, position)
        total += segment_duration
        position += entry_size
    duration = int((total * 1000 + movie_timescale // 2) // movie_timescale) if movie_timescale else 0
    return duration, error


def _codec_from_entry(entry_type: str) -> str:
    return VIDEO_CODECS.get(entry_type) or AUDIO_CODECS.get(entry_type) or entry_type


def _prefix_box_paths(children: list[Box], prefix: str) -> None:
    for child in children:
        child.path = f"{prefix}/{child.path}"
        _prefix_box_paths(child.children, prefix)


def _parse_stsd(fh: BinaryIO, box: Box) -> tuple[int, int, str, codecs.CodecInfo, int | None, int | None]:
    data = _payload(fh, box, limit=2 * 1024 * 1024)
    if len(data) < 8:
        raise ContainerError("BOX_MALFORMED", "stsd payload is truncated", offset=box.offset)
    declared = _u32(data, 4)
    position = 8
    actual = 0
    entry_type = ""
    info = codecs.CodecInfo()
    width = height = None
    while position + 8 <= len(data):
        entry_size = _u32(data, position)
        entry_type_candidate = data[position + 4:position + 8].decode("latin-1", "replace")
        if entry_size < 8 or position + entry_size > len(data):
            raise ContainerError("BOX_MALFORMED", "stsd sample entry has invalid size", offset=box.data_offset + position)
        if actual == 0:
            entry_type = entry_type_candidate
            entry_payload = position + 8
            if entry_type in VIDEO_CODECS:
                width = _u16(data, entry_payload + 24) if entry_payload + 26 <= len(data) else None
                height = _u16(data, entry_payload + 26) if entry_payload + 28 <= len(data) else None
                child_start = entry_payload + 78
            elif entry_type in AUDIO_CODECS:
                if entry_payload + 28 <= len(data):
                    version = _u16(data, entry_payload + 8)
                    info.channels = _u16(data, entry_payload + 16) or None
                    info.sample_rate = _u32(data, entry_payload + 24) >> 16 or None
                    child_start = entry_payload + 28 + (16 if version == 1 else 36 if version == 2 else 0)
                else:
                    child_start = len(data)
            else:
                child_start = position + entry_size
            child_end = position + entry_size
            if child_start < child_end:
                children = parse_boxes(fh, start=box.data_offset + child_start, end=box.data_offset + child_end)
                _prefix_box_paths(children, f"{box.path}/{entry_type_candidate}[0]")
                avcc = find_box(children, "avcC")
                hvcc = find_box(children, "hvcC")
                esds = find_box(children, "esds")
                if avcc is not None:
                    info = codecs.parse_avcc(_payload(fh, avcc, limit=1024 * 1024))
                    info.codec = VIDEO_CODECS.get(entry_type)
                    info.codec_box_path = avcc.path
                    info.box_offset = avcc.offset  # type: ignore[attr-defined]
                elif hvcc is not None:
                    info = codecs.parse_hvcc(_payload(fh, hvcc, limit=1024 * 1024))
                    info.codec = VIDEO_CODECS.get(entry_type)
                    info.codec_box_path = hvcc.path  # type: ignore[attr-defined]
                    info.box_offset = hvcc.offset  # type: ignore[attr-defined]
                elif esds is not None:
                    esds_info = codecs.parse_esds(_payload(fh, esds, limit=1024 * 1024))
                    info.sample_rate = info.sample_rate or esds_info.sample_rate
                    info.channels = info.channels or esds_info.channels
                    info.object_type = esds_info.object_type
                    info.codec_box_path = esds.path  # type: ignore[attr-defined]
                    info.box_offset = esds.offset  # type: ignore[attr-defined]
        actual += 1
        position += entry_size
    return declared, actual, entry_type, info, width, height


@dataclass
class _Tables:
    stts: list[tuple[int, int]] = field(default_factory=list)
    sizes: list[int] = field(default_factory=list)
    sample_count: int = 0
    stsc: list[tuple[int, int, int]] = field(default_factory=list)
    chunk_offsets: list[int] = field(default_factory=list)
    chunk_box: Box | None = None


def _parse_stts(data: bytes) -> tuple[list[tuple[int, int]], int]:
    if len(data) < 8:
        raise ContainerError("BOX_MALFORMED", "stts payload is truncated", offset=0)
    count = _u32(data, 4)
    entries: list[tuple[int, int]] = []
    total = 0
    for index in range(count):
        position = 8 + index * 8
        if position + 8 > len(data):
            break
        sample_count, delta = _u32(data, position), _u32(data, position + 4)
        entries.append((sample_count, delta))
        total += sample_count
    return entries, total


def _parse_stsz(data: bytes) -> tuple[int, list[int], int]:
    if len(data) < 12:
        raise ContainerError("BOX_MALFORMED", "stsz payload is truncated", offset=0)
    fixed_size, count = _u32(data, 4), _u32(data, 8)
    if fixed_size:
        return count, [fixed_size] * count, count
    available = max(0, (len(data) - 12) // 4)
    actual = min(count, available)
    sizes = [_u32(data, 12 + index * 4) for index in range(actual)]
    if actual < count:
        sizes.extend([0] * (count - actual))
    return count, sizes, actual


def _parse_stsc(data: bytes) -> list[tuple[int, int, int]]:
    if len(data) < 8:
        raise ContainerError("BOX_MALFORMED", "stsc payload is truncated", offset=0)
    count = _u32(data, 4)
    entries = []
    for index in range(count):
        position = 8 + index * 12
        if position + 12 > len(data):
            break
        entries.append((_u32(data, position), _u32(data, position + 4), _u32(data, position + 8)))
    return entries


def _parse_chunk_offsets(data: bytes, wide: bool) -> list[int]:
    if len(data) < 8:
        raise ContainerError("BOX_MALFORMED", "chunk offset table is truncated", offset=0)
    count = _u32(data, 4)
    width = 8 if wide else 4
    available = max(0, (len(data) - 8) // width)
    return [
        _u64(data, 8 + index * width) if wide else _u32(data, 8 + index * width)
        for index in range(min(count, available))
    ]


def _expand_durations(entries: list[tuple[int, int]], count: int) -> list[int]:
    values: list[int] = []
    for sample_count, delta in entries:
        take = min(sample_count, count - len(values))
        if take <= 0:
            break
        values.extend([delta] * take)
    if len(values) < count:
        values.extend([0] * (count - len(values)))
    return values


def _build_sample_ranges(tables: _Tables, timescale: int, file_size: int) -> list[SampleRange]:
    del timescale  # sample durations are stored in the surrounding tracks for drift checks
    if not tables.chunk_offsets or not tables.sizes:
        return []
    durations = _expand_durations(tables.stts, tables.sample_count)
    ranges: list[SampleRange] = []
    sample_index = 0
    stsc_index = 0
    for chunk_number, chunk_offset in enumerate(tables.chunk_offsets, start=1):
        while stsc_index + 1 < len(tables.stsc) and tables.stsc[stsc_index + 1][0] <= chunk_number:
            stsc_index += 1
        samples_per_chunk = tables.stsc[stsc_index][1] if tables.stsc else 0
        cursor = chunk_offset
        for _ in range(samples_per_chunk):
            if sample_index >= len(tables.sizes):
                break
            size = tables.sizes[sample_index]
            ranges.append(
                SampleRange(
                    offset=cursor,
                    size=size,
                    duration=durations[sample_index] if sample_index < len(durations) else 0,
                )
            )
            cursor += size
            sample_index += 1
    return ranges


@dataclass
class _Fragment:
    duration: int = 0
    samples: list[SampleRange] = field(default_factory=list)
    error: str | None = None
    box_path: str = ""
    box_offset: int = 0


def _parse_tfhd(data: bytes) -> dict:
    if len(data) < 8:
        raise ContainerError("BOX_MALFORMED", "tfhd payload is truncated", offset=0)
    flags = _u32(data, 0) & 0xFFFFFF
    track_id = _u32(data, 4)
    position = 8
    values: dict = {"track_id": track_id, "base_offset": 0, "default_duration": 0, "default_size": 0}
    if flags & 0x000001:
        if position + 8 > len(data):
            return values
        values["base_offset"] = _u64(data, position)
        position += 8
    if flags & 0x000002:
        position += 4
    if flags & 0x000008:
        if position + 4 <= len(data):
            values["default_duration"] = _u32(data, position)
        position += 4
    if flags & 0x000010:
        if position + 4 <= len(data):
            values["default_size"] = _u32(data, position)
        position += 4
    return values


def _parse_trex(fh: BinaryIO, moov: Box) -> dict[int, tuple[int, int]]:
    result: dict[int, tuple[int, int]] = {}
    mvex = moov.child("mvex")
    if mvex is None:
        return result
    for trex in mvex.children_of("trex"):
        data = _payload(fh, trex, limit=1024 * 1024)
        if len(data) >= 16:
            track_id = _u32(data, 4)
            result[track_id] = (_u32(data, 12), _u32(data, 16) if len(data) >= 20 else 0)
    return result


def _parse_trun(
    data: bytes,
    default_duration: int,
    default_size: int,
    base_offset: int,
    fallback_offset: int,
) -> tuple[_Fragment, int]:
    if len(data) < 8:
        raise ContainerError("BOX_MALFORMED", "trun payload is truncated", offset=0)
    version = data[0]
    flags = _u32(data, 0) & 0xFFFFFF
    sample_count = _u32(data, 4)
    position = 8
    data_offset = 0
    has_data_offset = bool(flags & 0x000001)
    if has_data_offset:
        if position + 4 > len(data):
            raise ContainerError("BOX_MALFORMED", "trun data offset is truncated", offset=0)
        data_offset = _i32(data, position)
        position += 4
    if flags & 0x000004:
        position += 4
    cursor = base_offset + data_offset if has_data_offset else fallback_offset
    fragment = _Fragment()
    missing_default = (flags & 0x000100) == 0 and default_duration == 0
    for _ in range(sample_count):
        duration = default_duration if (flags & 0x000100) == 0 else 0
        size = default_size if (flags & 0x000200) == 0 else 0
        if flags & 0x000100:
            if position + 4 > len(data):
                raise ContainerError("BOX_MALFORMED", "trun sample duration is truncated", offset=0)
            duration = _u32(data, position)
            position += 4
        if flags & 0x000200:
            if position + 4 > len(data):
                raise ContainerError("BOX_MALFORMED", "trun sample size is truncated", offset=0)
            size = _u32(data, position)
            position += 4
        if flags & 0x000400:
            position += 4
        if flags & 0x000800:
            position += 8 if version == 1 else 4
        if missing_default:
            fragment.error = "FRAG_NO_DEFAULT_DURATION"
        fragment.samples.append(SampleRange(cursor, size, duration))
        fragment.duration += duration
        cursor += size
    return fragment, cursor


def _apply_fragments(
    fh: BinaryIO, boxes: list[Box], tracks: dict[int, TrackInfo], trex: dict[int, tuple[int, int]]
) -> bool:
    fragmented = False
    fragment_durations: dict[int, int] = {}
    for moof in [box for box in boxes if box.type == "moof"]:
        fragmented = True
        for traf in moof.children_of("traf"):
            tfhd = traf.child("tfhd")
            if tfhd is None:
                continue
            values = _parse_tfhd(_payload(fh, tfhd, limit=1024 * 1024))
            track = tracks.get(values["track_id"])
            if track is None:
                continue
            tfdt = traf.child("tfdt")
            if tfdt is not None and track.base_media_decode_time is None:
                data = _payload(fh, tfdt, limit=1024 * 1024)
                if len(data) >= 8:
                    time = _u64(data, 4) if data[0] == 1 and len(data) >= 12 else _u32(data, 4)
                    track.base_media_decode_time = time
            trex_duration, trex_size = trex.get(track.track_id, (0, 0))
            default_duration = values["default_duration"] or trex_duration
            default_size = values["default_size"] or trex_size
            base_offset = values["base_offset"] or moof.offset
            fallback = values["base_offset"] or moof.end
            for trun in traf.children_of("trun"):
                fragment, fallback = _parse_trun(
                    _payload(fh, trun, limit=16 * 1024 * 1024),
                    default_duration,
                    default_size,
                    base_offset,
                    fallback,
                )
                fragment.box_path = trun.path
                fragment.box_offset = trun.offset
                track.sample_ranges.extend(fragment.samples)
                track.sample_count += len(fragment.samples)
                fragment_durations[track.track_id] = fragment_durations.get(track.track_id, 0) + fragment.duration
                if fragment.error:
                    track.fragment_error = fragment.error
                    track.fragment_box_path = fragment.box_path
                    track.fragment_box_offset = fragment.box_offset
    for track in tracks.values():
        if (
            not track.has_stbl_samples
            and track.duration_source != "elst"
            and track.track_id in fragment_durations
            and track.timescale
        ):
            track.duration_source = "fragments"
            track.duration_ms = int(
                (fragment_durations[track.track_id] * 1000 + track.timescale // 2) // track.timescale
            )
    return fragmented


def _parse_track(fh: BinaryIO, trak: Box, movie_timescale: int, file_size: int) -> TrackInfo:
    track = TrackInfo()
    tkhd = trak.child("tkhd")
    if tkhd is not None:
        track.track_id, width, height = _parse_tkhd(_payload(fh, tkhd, limit=1024 * 1024))
        track.width, track.height = width, height
    mdia = trak.child("mdia")
    if mdia is None:
        raise ContainerError("BOX_MALFORMED", "trak is missing mdia", offset=trak.offset)
    mdhd = mdia.child("mdhd")
    if mdhd is not None:
        track.timescale, raw_duration, track.language = _parse_mdhd(_payload(fh, mdhd, limit=1024 * 1024))
        track.duration_ms = int((raw_duration * 1000 + track.timescale // 2) // track.timescale) if track.timescale else 0
        track.duration_box_path = mdhd.path
        track.duration_box_offset = mdhd.offset
    hdlr = mdia.child("hdlr")
    if hdlr is not None:
        track.handler_type, _ = _parse_hdlr(_payload(fh, hdlr, limit=1024 * 1024))
    track.handler = HANDLER_NAMES.get(track.handler_type, track.handler_type or "unknown")
    edts = trak.child("edts")
    elst = edts.child("elst") if edts is not None else None
    if elst is not None:
        duration, error = _parse_elst(_payload(fh, elst, limit=4 * 1024 * 1024), movie_timescale)
        if error is None:
            track.duration_ms = duration
            track.duration_source = "elst"
            track.duration_box_path = elst.path
            track.duration_box_offset = elst.offset
    minf = mdia.child("minf")
    if minf is None:
        raise ContainerError("BOX_MALFORMED", "mdia is missing minf", offset=mdia.offset)
    stbl = minf.child("stbl")
    if stbl is None:
        raise ContainerError("BOX_MALFORMED", "minf is missing stbl", offset=minf.offset)
    stsd = stbl.child("stsd")
    if stsd is not None:
        declared, actual, entry_type, info, width, height = _parse_stsd(fh, stsd)
        track.stsd_count, track.stsd_actual = declared, actual
        track.codec = info.codec or _codec_from_entry(entry_type)
        track.profile, track.level = info.profile, info.level
        track.sample_rate = info.sample_rate or track.sample_rate
        track.channels = info.channels or track.channels
        track.width = track.width or width
        track.height = track.height or height
        track.stsd_box_path, track.stsd_box_offset = stsd.path, stsd.offset
        track.codec_box_path, track.codec_box_offset = info.codec_box_path or stsd.path, info.box_offset or stsd.offset
    tables = _Tables()
    stts = stbl.child("stts")
    if stts is not None:
        tables.stts, table_samples = _parse_stts(_payload(fh, stts, limit=16 * 1024 * 1024))
        tables.sample_count = max(tables.sample_count, table_samples)
    if stbl.child("stsz") is not None:
        tables.sample_count, tables.sizes, _ = _parse_stsz(_payload(fh, stbl.child("stsz"), limit=16 * 1024 * 1024))
    if stbl.child("stsc") is not None:
        tables.stsc = _parse_stsc(_payload(fh, stbl.child("stsc"), limit=4 * 1024 * 1024))
    chunk_box = stbl.child("stco") or stbl.child("co64")
    if chunk_box is not None:
        tables.chunk_box = chunk_box
        tables.chunk_offsets = _parse_chunk_offsets(
            _payload(fh, chunk_box, limit=16 * 1024 * 1024), chunk_box.type == "co64"
        )
        track.chunk_box_path, track.chunk_box_offset = chunk_box.path, chunk_box.offset
        track.invalid_chunk_offsets = sum(1 for offset in tables.chunk_offsets if offset >= file_size)
    track.has_stbl_samples = tables.sample_count > 0
    track.sample_count = tables.sample_count
    track.sample_ranges = _build_sample_ranges(tables, track.timescale, file_size)
    return track


def parse_isobmff(source: str | os.PathLike[str] | BinaryIO) -> IsoInfo:
    owns_file = not hasattr(source, "read")
    if owns_file:
        try:
            fh = open(source, "rb")
        except PermissionError as exc:
            raise MediaError("PERMISSION_DENIED", str(exc), path=str(source), exit_code=2) from exc
        except OSError as exc:
            raise MediaError("IO_ERROR", str(exc), path=str(source), exit_code=2) from exc
    else:
        fh = source  # type: ignore[assignment]
    try:
        fh.seek(0, os.SEEK_END)
        file_size = fh.tell()
        boxes = parse_boxes(fh)
        moov = next((box for box in boxes if box.type == "moov"), None)
        if moov is None:
            raise ContainerError("CONTAINER_MALFORMED", "ISO BMFF file has no moov box", offset=0)
        ftyp = next((box for box in boxes if box.type == "ftyp"), None)
        major_brand = ""
        brands: list[str] = []
        if ftyp is not None:
            data = _payload(fh, ftyp, limit=1024 * 1024)
            if len(data) >= 4:
                major_brand = data[:4].decode("latin-1", "replace").strip()
            brands = [
                data[position:position + 4].decode("latin-1", "replace").strip()
                for position in range(8, len(data) - 3, 4)
            ]
        movie_timescale, movie_duration = 1000, 0
        mvhd = moov.child("mvhd")
        if mvhd is not None:
            movie_timescale, movie_duration = _parse_mvhd(_payload(fh, mvhd, limit=1024 * 1024))
        tracks = [
            _parse_track(fh, trak, movie_timescale or 1000, file_size)
            for trak in moov.children_of("trak")
        ]
        trex = _parse_trex(fh, moov)
        fragmented = _apply_fragments(fh, boxes, {track.track_id: track for track in tracks}, trex)
        duration_ms = max((track.duration_ms for track in tracks if track.handler_type != "tmcd"), default=0)
        if duration_ms == 0 and movie_timescale:
            duration_ms = int((movie_duration * 1000 + movie_timescale // 2) // movie_timescale)
        extension = os.path.splitext(os.fspath(source))[1].lower() if owns_file else ""
        container_format = "mov" if major_brand.startswith("qt") or extension == ".mov" else "mp4"
        return IsoInfo(
            path=os.fspath(source) if owns_file else getattr(source, "name", "<stream>"),
            file_size=file_size,
            brand=major_brand,
            brands=brands,
            format=container_format,
            movie_timescale=movie_timescale or 1000,
            duration_ms=duration_ms,
            fragmented=fragmented,
            tracks=tracks,
            moov_path=moov.path,
            moov_offset=moov.offset,
        )
    finally:
        if owns_file:
            fh.close()
