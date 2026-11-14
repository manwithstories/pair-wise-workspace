"""Compose a playable-enough MP4 from a repaired Matroska timeline."""

from __future__ import annotations

from dataclasses import dataclass

from mkvtool.errors import UnsupportedTrackError
from mkvtool.mkv.demux import Track
from mkvtool.mkv.timeline import NS_PER_MS, RewrittenSample, TimelineResult, TrackTimeline
from mkvtool.mp4.boxes import (
    IDENTITY_MATRIX,
    avc1_sample_entry,
    box,
    build_ctts,
    build_stco,
    build_stsc,
    build_stss,
    build_stsd,
    build_stsz,
    build_stts,
    full_box,
    ftyp,
    language_code,
    mdat,
    mp4a_sample_entry,
    u16,
    u32,
)

MOVIE_TIMESCALE = 1000


@dataclass
class MuxTrack:
    source_number: int
    mp4_track_id: int
    handler: str
    timescale: int
    samples: list[RewrittenSample]
    dts_ticks: list[int]
    durations_ticks: list[int]
    ctts_ticks: list[int]
    sync_samples: list[int]
    chunk_offsets: list[int]
    width: int = 0
    height: int = 0
    codec_private: bytes = b""
    sample_rate: int = 0
    channels: int = 0
    language: str = "und"


@dataclass
class MuxResult:
    data: bytes
    tracks: list[MuxTrack]
    mdat_offset: int
    moov_offset: int


def _to_ticks(ns: int, timescale: int) -> int:
    return int(round(ns * timescale / 1_000_000_000))


def _track_timescale(track: Track) -> int:
    if track.kind == "video":
        return 1000
    if track.sample_rate and track.sample_rate > 0:
        return int(round(track.sample_rate))
    return 48000


def _sample_asc(track: Track) -> bytes:
    return bytes(track.codec_private) or b"\x11\x90"


def _stbl(track: MuxTrack) -> bytes:
    samples = track.samples
    if not samples:
        raise UnsupportedTrackError("cannot write an empty track")
    if track.handler == "vide":
        entry = avc1_sample_entry(
            track.width or 1920,
            track.height or 1080,
            track.codec_private or b"\x01\x42\x00\x1e\xff",
        )
    else:
        entry = mp4a_sample_entry(
            track.channels or 2,
            track.sample_rate or track.timescale,
            track.codec_private or b"\x11\x90",
        )
    tables = [
        build_stsd(entry),
        build_stts(track.durations_ticks),
    ]
    if any(offset > 0 for offset in track.ctts_ticks):
        tables.append(build_ctts(track.ctts_ticks))
    if track.handler == "vide":
        tables.append(build_stss(track.sync_samples))
    tables.extend([
        build_stsc(len(samples), 1),
        build_stsz([len(sample.data) for sample in samples]),
        build_stco(track.chunk_offsets),
    ])
    return box("stbl", b"".join(tables))


def _trak(track: MuxTrack, track_duration: int) -> bytes:
    tkhd_duration = int(round(track_duration * MOVIE_TIMESCALE / track.timescale))
    volume = 0x0100 if track.handler == "soun" else 0
    display_width = (track.width or 1920) << 16 if track.handler == "vide" else 0
    display_height = (track.height or 1080) << 16 if track.handler == "vide" else 0
    tkhd_payload = (
        u32(0) + u32(0) + u32(track.mp4_track_id) + u32(0) + u32(tkhd_duration)
        + b"\x00" * 8 + u16(0) + u16(0) + u16(volume) + u16(0) + IDENTITY_MATRIX
        + u32(display_width) + u32(display_height)
    )
    tkhd = full_box("tkhd", 0, 3, tkhd_payload)
    mdhd_payload = (
        u32(0) + u32(0) + u32(track.timescale) + u32(track_duration)
        + u16(language_code(track.language)) + u16(0)
    )
    mdhd = full_box("mdhd", 0, 0, mdhd_payload)
    hdlr = full_box(
        "hdlr", 0, 0, u32(0) + track.handler.encode("ascii") + b"\x00" * 12
        + (b"VideoHandler\x00" if track.handler == "vide" else b"SoundHandler\x00")
    )
    if track.handler == "vide":
        media_header = full_box("vmhd", 0, 1, u16(0) + u16(0) + u16(0) + u16(0))
    else:
        media_header = full_box("smhd", 0, 0, u16(0) + u16(0))
    url = full_box("url ", 0, 1, b"")
    dref = full_box("dref", 0, 0, u32(1) + url)
    dinf = box("dinf", dref)
    minf = box("minf", media_header + dinf + _stbl(track))
    mdia = box("mdia", mdhd + hdlr + minf)
    return box("trak", tkhd + mdia)


def _build_track(
    timeline: TrackTimeline,
    track: Track,
    mp4_track_id: int,
    data_start: int,
    offset: int,
) -> tuple[MuxTrack, int]:
    timescale = _track_timescale(track)
    samples = timeline.samples
    dts_ticks: list[int] = []
    durations: list[int] = []
    ctts: list[int] = []
    previous = -1
    first_dts_ns = samples[0].dts_ns
    for sample in samples:
        dts = _to_ticks(sample.dts_ns - first_dts_ns, timescale)
        if dts <= previous:
            dts = previous + 1
        pts = _to_ticks(sample.pts_ns - first_dts_ns, timescale)
        offset_ticks = max(0, pts - dts)
        dts_ticks.append(dts)
        ctts.append(offset_ticks)
        previous = dts
    for index, sample in enumerate(samples):
        if index + 1 < len(dts_ticks):
            duration = dts_ticks[index + 1] - dts_ticks[index]
        else:
            duration = _to_ticks(sample.duration_ns, timescale)
        durations.append(max(1, duration))
    chunk_offsets: list[int] = []
    cursor = offset
    for sample in samples:
        chunk_offsets.append(data_start + cursor)
        cursor += len(sample.data)
    sync = [index + 1 for index, sample in enumerate(samples) if sample.keyframe]
    mux_track = MuxTrack(
        source_number=track.number,
        mp4_track_id=mp4_track_id,
        handler="vide" if track.kind == "video" else "soun",
        timescale=timescale,
        samples=samples,
        dts_ticks=dts_ticks,
        durations_ticks=durations,
        ctts_ticks=ctts,
        sync_samples=sync,
        chunk_offsets=chunk_offsets,
        width=track.pixel_width or 0,
        height=track.pixel_height or 0,
        codec_private=track.codec_private,
        sample_rate=timescale,
        channels=track.channels or 0,
        language=track.language,
    )
    return mux_track, cursor


def mux_timeline(timeline: TimelineResult) -> MuxResult:
    supported = [track for track in timeline.demux.tracks if track.supported]
    if not supported:
        raise UnsupportedTrackError("no supported track can be muxed to MP4")
    prefix = ftyp()
    payload_parts: list[bytes] = []
    built_specs: list[tuple[TrackTimeline, Track, int, int, int]] = []
    data_cursor = 0
    for mp4_track_id, track in enumerate(supported, 1):
        track_timeline = timeline.tracks[track.number]
        built_specs.append((track_timeline, track, mp4_track_id, data_cursor, 0))
        for sample in track_timeline.samples:
            payload_parts.append(sample.data)
            data_cursor += len(sample.data)
    payload = b"".join(payload_parts)
    mdat_bytes = mdat(payload)
    data_start = len(prefix) + 8
    tracks: list[MuxTrack] = []
    offset = 0
    for track_timeline, track, mp4_track_id, track_offset, _ in built_specs:
        mux_track, _ = _build_track(
            track_timeline, track, mp4_track_id, data_start, track_offset
        )
        tracks.append(mux_track)
        offset += sum(len(sample.data) for sample in track_timeline.samples)
    durations_movie = [
        int(round(sum(track.durations_ticks) * MOVIE_TIMESCALE / track.timescale))
        for track in tracks
    ]
    movie_duration = max(durations_movie, default=0)
    mvhd_payload = (
        u32(0) + u32(0) + u32(MOVIE_TIMESCALE) + u32(movie_duration)
        + u32(0x00010000) + u16(0x0100) + b"\x00" * 10 + IDENTITY_MATRIX
        + b"\x00" * 24 + u32(len(tracks) + 1)
    )
    traks = [
        _trak(track, sum(track.durations_ticks))
        for track in tracks
    ]
    moov = box("moov", full_box("mvhd", 0, 0, mvhd_payload) + b"".join(traks))
    result = prefix + mdat_bytes + moov
    return MuxResult(
        data=result,
        tracks=tracks,
        mdat_offset=len(prefix),
        moov_offset=len(prefix) + len(mdat_bytes),
    )


def mux_file(timeline: TimelineResult, path: str) -> MuxResult:
    result = mux_timeline(timeline)
    with open(path, "wb") as handle:
        handle.write(result.data)
    return result
