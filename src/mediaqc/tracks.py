"""Track and sample-table parsing for ISO BMFF files."""

from __future__ import annotations

import struct
from typing import Any

from .bmff import Box, BoxError, child_boxes, payload
from .codec import parse_sample_entry
from .model import Finding, Sample, Track

MAX_EXPANDED_SAMPLES = 100_000
MAX_TABLE_ENTRIES = 100_000


def _child(boxes: list[Box], name: str) -> Box | None:
    wanted = name.encode("latin-1")
    return next((box for box in boxes if box.type == wanted), None)


def _tag_track(findings: list[Finding], track_index: int) -> None:
    for finding in findings:
        if finding.track_index is None:
            finding.track_index = track_index


def _u32(data: bytes, offset: int) -> int:
    return struct.unpack(">I", data[offset:offset + 4])[0]


def _i32(data: bytes, offset: int) -> int:
    return struct.unpack(">i", data[offset:offset + 4])[0]


def _prefix(handle, box: Box, length: int) -> bytes:
    handle.seek(box.payload_offset)
    return handle.read(min(length, box.payload_size))


def movie_timescale(handle, moov: Box) -> tuple[int, int]:
    mvhd = _child(child_boxes(handle, moov), "mvhd")
    if mvhd is None:
        return 0, moov.offset
    data = payload(handle, mvhd)
    if len(data) < 20:
        raise BoxError("truncated mvhd", mvhd.offset)
    version = data[0]
    position = 20 if version == 1 else 12
    return _u32(data, position), mvhd.payload_offset + position


def _parse_mdhd(handle, box: Box) -> tuple[int, int, int]:
    data = payload(handle, box)
    if len(data) < 20:
        raise BoxError("truncated mdhd", box.offset)
    if data[0] == 1:
        if len(data) < 32:
            raise BoxError("truncated version 1 mdhd", box.offset)
        timescale = _u32(data, 20)
        duration = struct.unpack(">Q", data[24:32])[0]
        field = box.payload_offset + 20
    else:
        timescale = _u32(data, 12)
        duration = _u32(data, 16)
        field = box.payload_offset + 12
    return timescale, duration, field


def _parse_tkhd(handle, box: Box | None) -> tuple[int, int | None, int | None]:
    if box is None:
        return 0, None, None
    data = payload(handle, box)
    if len(data) < 24:
        return 0, None, None
    track_id = _u32(data, 20 if data[0] == 1 else 12)
    width = height = None
    if len(data) >= 8:
        width = _u32(data, len(data) - 8) >> 16
        height = _u32(data, len(data) - 4) >> 16
    return track_id, width, height


def _parse_hdlr(handle, box: Box | None) -> str:
    if box is None:
        return "unknown"
    data = payload(handle, box)
    if len(data) < 12:
        return "unknown"
    handler = data[8:12]
    return {"vide": "video", "soun": "audio", "sbtl": "subtitle", "text": "text"}.get(
        handler.decode("latin-1", "replace"), handler.decode("latin-1", "replace")
    )


def _parse_elst(handle, box: Box) -> tuple[list[dict[str, int]], list[Finding]]:
    data = payload(handle, box)
    findings: list[Finding] = []
    if len(data) < 8:
        return [], [Finding("BOX_BOUNDARY_INVALID", "error", "truncated elst", str(len(data)),
                            offset=box.offset)]
    version = data[0]
    count = _u32(data, 4)
    position = 8
    entry_size = 20 if version == 1 else 12
    edits: list[dict[str, int]] = []
    for index in range(min(count, MAX_TABLE_ENTRIES, (len(data) - 8) // entry_size)):
        entry_offset = box.payload_offset + position
        if version == 1:
            segment = struct.unpack(">Q", data[position:position + 8])[0]
            media_time = struct.unpack(">q", data[position + 8:position + 16])[0]
            rate = struct.unpack(">i", data[position + 16:position + 20])[0] >> 16
        else:
            segment = _u32(data, position)
            media_time = _i32(data, position + 4)
            rate = struct.unpack(">h", data[position + 8:position + 10])[0]
        edits.append({"segment_duration": segment, "media_time": media_time,
                      "rate": rate, "offset": entry_offset})
        position += entry_size
    return edits, findings


def _parse_stts(handle, box: Box | None) -> tuple[list[tuple[int, int]], int, int, bool]:
    if box is None:
        return [], 0, 0, False
    if box.payload_size < 8:
        return [], 0, 0, False
    data = _prefix(handle, box, 8)
    if len(data) < 8:
        return [], 0, 0, False
    count = _u32(data, 4)
    available = (box.payload_size - 8) // 8
    usable = min(count, available, MAX_TABLE_ENTRIES)
    data = _prefix(handle, box, 8 + usable * 8)
    entries = [( _u32(data, 8 + i * 8), _u32(data, 12 + i * 8)) for i in range(usable)]
    total = sum(item[0] for item in entries)
    return entries, total, box.payload_offset + 8, count > usable


def _parse_ctts(handle, box: Box | None) -> tuple[list[tuple[int, int, int]], list[Finding], bool]:
    if box is None:
        return [], [], False
    if box.payload_size < 8:
        return [], [], False
    data = _prefix(handle, box, 8)
    if len(data) < 8:
        return [], [], False
    version = data[0]
    count = _u32(data, 4)
    available = (box.payload_size - 8) // 8
    usable = min(count, available, MAX_TABLE_ENTRIES)
    data = _prefix(handle, box, 8 + usable * 8)
    entries: list[tuple[int, int, int]] = []
    findings: list[Finding] = []
    for index in range(usable):
        position = 8 + index * 8
        sample_count = _u32(data, position)
        if version == 1:
            offset = _i32(data, position + 4)
        else:
            offset = _u32(data, position + 4)
        entry_offset = box.payload_offset + position
        entries.append((sample_count, offset, entry_offset))
        if offset < 0:
            findings.append(Finding(
                "NEGATIVE_CTTS", "warning", "composition time offset is negative",
                f"ctts entry {index}: {offset}", offset=entry_offset + 4,
            ))
    return entries, findings, count > usable


def _parse_stsc(handle, box: Box | None) -> tuple[list[tuple[int, int, int]], int, bool]:
    if box is None:
        return [], 0, False
    if box.payload_size < 8:
        return [], 0, False
    data = _prefix(handle, box, 8)
    if len(data) < 8:
        return [], 0, False
    count = _u32(data, 4)
    available = (box.payload_size - 8) // 12
    usable = min(count, available, MAX_TABLE_ENTRIES)
    data = _prefix(handle, box, 8 + usable * 12)
    entries = [(
        _u32(data, 8 + i * 12), _u32(data, 12 + i * 12), _u32(data, 16 + i * 12)
    ) for i in range(usable)]
    return entries, box.payload_offset + 8, count > usable


def _parse_stsz(handle, box: Box | None) -> tuple[int, int, list[int] | None, int, bool]:
    if box is None:
        return 0, 0, None, 0, False
    if box.payload_size < 12:
        return 0, 0, None, box.payload_offset, False
    data = _prefix(handle, box, 12)
    if len(data) < 12:
        return 0, 0, None, box.payload_offset, False
    constant = _u32(data, 4)
    count = _u32(data, 8)
    if constant:
        return constant, count, None, box.payload_offset + 4, False
    available = min((box.payload_size - 12) // 4, MAX_EXPANDED_SAMPLES)
    if count > MAX_EXPANDED_SAMPLES:
        return 0, count, None, box.payload_offset + 8, True
    usable = min(count, available)
    data = _prefix(handle, box, 12 + usable * 4)
    sizes = [_u32(data, 12 + i * 4) for i in range(usable)]
    return 0, count, sizes, box.payload_offset + 8, count > usable


def _parse_chunk_offsets(handle, box: Box | None) -> tuple[list[int], int, bool]:
    if box is None:
        return [], 0, False
    if box.payload_size < 8:
        return [], 0, False
    data = _prefix(handle, box, 8)
    if len(data) < 8:
        return [], 0, False
    count = _u32(data, 4)
    width = 8 if box.type == b"co64" else 4
    available = (box.payload_size - 8) // width
    usable = min(count, available, MAX_TABLE_ENTRIES)
    data = _prefix(handle, box, 8 + usable * width)
    if width == 8:
        offsets = [struct.unpack(">Q", data[8 + i * 8:16 + i * 8])[0] for i in range(usable)]
    else:
        offsets = [_u32(data, 8 + i * 4) for i in range(usable)]
    return offsets, box.payload_offset + 8, count > usable


def _parse_stss(handle, box: Box | None, sample_count: int) -> tuple[list[int], bool]:
    if box is None:
        return list(range(1, min(sample_count, MAX_EXPANDED_SAMPLES) + 1)), False
    if box.payload_size < 8:
        return [], False
    data = _prefix(handle, box, 8)
    if len(data) < 8:
        return [], False
    count = _u32(data, 4)
    available = (box.payload_size - 8) // 4
    usable = min(count, available, MAX_EXPANDED_SAMPLES)
    data = _prefix(handle, box, 8 + usable * 4)
    return [_u32(data, 8 + i * 4) for i in range(usable)], count > usable


def _parse_stsd(handle, box: Box | None, track_index: int):
    if box is None:
        return None
    data = payload(handle, box)
    if len(data) < 8:
        return None
    count = _u32(data, 4)
    position = 8
    for _ in range(min(count, 32)):
        if position + 8 > len(data):
            break
        size, kind = struct.unpack(">I4s", data[position:position + 8])
        if size < 8 or position + size > len(data):
            break
        entry = Box(kind, box.payload_offset + position, size, 8,
                    box.payload_offset + position + size)
        return parse_sample_entry(handle, entry, track_index)
    return None


def _timeline(
    sample_count: int,
    constant_size: int,
    sizes: list[int] | None,
    chunk_offsets: list[int],
    stsc: list[tuple[int, int, int]],
    stts: list[tuple[int, int]],
    ctts: list[tuple[int, int, int]],
    sync_numbers: list[int],
    has_sync_table: bool,
) -> tuple[list[Sample], list[tuple[int, int, int]], bool]:
    if sample_count <= 0 or sample_count > MAX_EXPANDED_SAMPLES:
        return [], [], sample_count > MAX_EXPANDED_SAMPLES
    if sizes is None and constant_size <= 0:
        return [], [], True
    duration_runs = [[count, delta] for count, delta in stts]
    ctts_runs = [[count, offset] for count, offset, _ in ctts]
    duration_index = ctts_index = 0
    dts = 0
    cts_offset = 0
    sync_set = set(sync_numbers)
    use_sync_table = has_sync_table
    samples: list[Sample] = []
    chunks: list[tuple[int, int, int]] = []
    cursor = 0
    stsc_index = 0
    for chunk_number, chunk_offset in enumerate(chunk_offsets, 1):
        spc = 0
        while stsc_index + 1 < len(stsc) and stsc[stsc_index + 1][0] <= chunk_number:
            stsc_index += 1
        if stsc and stsc[stsc_index][0] <= chunk_number:
            spc = stsc[stsc_index][1]
        chunk_start = chunk_offset
        for _ in range(spc):
            if cursor >= sample_count:
                break
            size = constant_size if sizes is None else (sizes[cursor] if cursor < len(sizes) else 0)
            while duration_index < len(duration_runs) and duration_runs[duration_index][0] <= 0:
                duration_index += 1
            if duration_index < len(duration_runs):
                duration = duration_runs[duration_index][1]
                duration_runs[duration_index][0] -= 1
            else:
                duration = 0
            while ctts_index < len(ctts_runs) and ctts_runs[ctts_index][0] <= 0:
                ctts_index += 1
            if ctts_index < len(ctts_runs):
                cts_offset = ctts_runs[ctts_index][1]
                ctts_runs[ctts_index][0] -= 1
            sample_number = cursor + 1
            sync = sample_number in sync_set if use_sync_table else True
            samples.append(Sample(cursor, dts, duration, chunk_offset, size,
                                  dts + cts_offset, sync, chunk_number))
            dts += duration
            chunk_offset += size
            cursor += 1
        if chunk_offset > chunk_start:
            chunks.append((chunk_start, chunk_offset, chunk_number))
    return samples, chunks, False


def _read_track(handle, trak: Box, index: int, movie_ts: int) -> tuple[Track, list[Finding]]:
    findings: list[Finding] = []
    children = child_boxes(handle, trak)
    track_id, tkhd_width, tkhd_height = _parse_tkhd(handle, _child(children, "tkhd"))
    mdia = _child(children, "mdia")
    if mdia is None:
        raise BoxError("trak has no mdia", trak.offset)
    media_children = child_boxes(handle, mdia)
    mdhd = _child(media_children, "mdhd")
    if mdhd is None:
        raise BoxError("mdia has no mdhd", mdia.offset)
    timescale, duration, timescale_offset = _parse_mdhd(handle, mdhd)
    kind = _parse_hdlr(handle, _child(media_children, "hdlr"))
    track = Track(index=index, track_id=track_id, kind=kind,
                  timescale=timescale, duration=duration, width=tkhd_width, height=tkhd_height)
    if timescale == 0:
        findings.append(Finding("TIMESCALE_ZERO", "error", "track timescale is zero",
                                "mdhd.timescale=0", index, timescale_offset))
    edts = _child(children, "edts")
    if edts is not None:
        elst = _child(child_boxes(handle, edts), "elst")
        if elst is not None:
            track.edits, edit_findings = _parse_elst(handle, elst)
            _tag_track(edit_findings, index)
            findings.extend(edit_findings)
    minf = _child(media_children, "minf")
    if minf is None:
        raise BoxError("mdia has no minf", mdia.offset)
    stbl = _child(child_boxes(handle, minf), "stbl")
    if stbl is None:
        raise BoxError("minf has no stbl", minf.offset)
    tables = child_boxes(handle, stbl)
    codec_info = _parse_stsd(handle, _child(tables, "stsd"), index)
    if codec_info is not None:
        track.codec = codec_info.codec
        track.sample_entry = codec_info.entry_type
        track.width = codec_info.width or track.width
        track.height = codec_info.height or track.height
        track.sample_rate = codec_info.sample_rate
        track.channels = codec_info.channels
        track.codec_details = codec_info.details
        findings.extend(codec_info.findings)
    stts, stts_total, stts_offset, stts_cut = _parse_stts(handle, _child(tables, "stts"))
    ctts, ctts_findings, ctts_cut = _parse_ctts(handle, _child(tables, "ctts"))
    _tag_track(ctts_findings, index)
    findings.extend(ctts_findings)
    stsc, stsc_offset, stsc_cut = _parse_stsc(handle, _child(tables, "stsc"))
    stsc_invalid = any(first_chunk < 1 or samples_per_chunk < 1
                       for first_chunk, samples_per_chunk, _ in stsc)
    constant, sample_count, sizes, stsz_offset, stsz_cut = _parse_stsz(handle, _child(tables, "stsz"))
    chunks = _child(tables, "co64") or _child(tables, "stco")
    chunk_offsets, stco_offset, stco_cut = _parse_chunk_offsets(handle, chunks)
    stss_box = _child(tables, "stss")
    sync_numbers, stss_cut = _parse_stss(handle, stss_box, sample_count)
    track.sample_count = sample_count
    track.sync_samples = sync_numbers
    track.table_facts = {
        "mdhd_offset": mdhd.offset,
        "stts_sample_count": stts_total,
        "stts_entry_count": len(stts),
        "stts_offset": stts_offset,
        "stsz_sample_count": sample_count,
        "stsz_sample_size": constant,
        "stsz_offset": stsz_offset,
        "chunk_offset_count": len(chunk_offsets),
        "stco_offset": stco_offset,
        "stsc_entry_count": len(stsc),
        "stsc_offset": stsc_offset,
        "stsc_invalid": stsc_invalid,
        "has_ctts": bool(ctts),
        "ctts_negative_offsets": [entry[2] + 4 for entry in ctts if entry[1] < 0],
        "has_edits": bool(track.edits),
        "has_stss": stss_box is not None,
        "sync_sample_count": len(sync_numbers) if stss_box is not None else sample_count,
        "empty_edit_offsets": [edit["offset"] for edit in track.edits
                               if edit["media_time"] < 0],
        "table_truncated": stts_cut or ctts_cut or stsc_cut or stsz_cut or stco_cut or stss_cut,
    }
    if track.table_facts["table_truncated"]:
        findings.append(Finding(
            "SAMPLE_COUNT_HIGH", "info", "sample table exceeds expansion threshold",
            f"sample_count={sample_count}; entries kept without expansion",
            index, stsz_offset,
        ))
    if stsc_invalid:
        findings.append(Finding(
            "STSC_INVALID", "error", "stsc contains an invalid chunk mapping",
            "first_chunk or samples_per_chunk is below 1", index, stsc_offset,
        ))
    samples, chunk_ranges, timeline_cut = _timeline(
        sample_count, constant, sizes, chunk_offsets, stsc, stts, ctts,
        sync_numbers, stss_box is not None,
    )
    track.samples = samples
    track.chunk_ranges = chunk_ranges
    track.samples_truncated = timeline_cut
    track.table_facts["timeline_truncated"] = timeline_cut
    if timeline_cut and not track.table_facts["table_truncated"]:
        findings.append(Finding(
            "SAMPLE_COUNT_HIGH", "info", "sample count exceeds timeline expansion threshold",
            f"sample_count={sample_count}; timeline was not expanded", index, stsz_offset,
        ))
    timeline_ticks = sum(sample.duration for sample in samples)
    if duration == 0 and timescale:
        track.duration = timeline_ticks
    track.duration_ms = track.duration / timescale * 1000 if timescale else 0.0
    if track.edits:
        scale = movie_ts or timescale
        if scale:
            accumulated = 0
            for edit in track.edits:
                if edit["media_time"] < 0:
                    track.presentation_start_ms = (accumulated + edit["segment_duration"]) / scale * 1000
                accumulated += edit["segment_duration"]
            track.presentation_duration_ms = accumulated / scale * 1000
        else:
            track.presentation_duration_ms = track.duration_ms
    else:
        track.presentation_duration_ms = track.duration_ms
    return track, findings


def parse_mp4_tracks(handle, scan) -> tuple[list[Track], list[Finding], int]:
    moov = scan.top("moov")
    if moov is None:
        return [], [Finding("MOOV_MISSING", "error", "top-level moov box is missing",
                            "cannot locate track metadata", offset=0)], 0
    try:
        movie_ts, _ = movie_timescale(handle, moov)
    except (BoxError, struct.error) as exc:
        movie_ts = 0
        return [], [Finding("BOX_BOUNDARY_INVALID", "error", "mvhd parse failed", str(exc),
                            offset=moov.offset)], 0
    tracks: list[Track] = []
    findings: list[Finding] = []
    try:
        traks = [box for box in child_boxes(handle, moov) if box.type == b"trak"]
    except BoxError as exc:
        return [], [Finding("BOX_BOUNDARY_INVALID", "error", "moov child parse failed",
                            str(exc), offset=exc.offset)], movie_ts
    for index, trak in enumerate(traks):
        try:
            track, track_findings = _read_track(handle, trak, index, movie_ts)
            tracks.append(track)
            findings.extend(track_findings)
        except (BoxError, struct.error, ValueError) as exc:
            findings.append(Finding("TRACK_PARSE_ERROR", "error", "track could not be parsed",
                                    str(exc), index, trak.offset))
    return tracks, findings, movie_ts
