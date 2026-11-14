"""Shared moov/trak construction for plain and fragmented outputs."""

from __future__ import annotations

import struct

import numpy as np

from mkvtomp4.mp4.boxes import box, container, descriptor, full_box, u16, u32, u64
from mkvtomp4.mp4.index import stbl_empty, stbl_plain
from mkvtomp4.timing.normalize import NormalizedTrack, NormalizedMedia
from mkvtomp4.mkv.tracks import TRACK_TYPE_AUDIO

MATRIX = struct.pack(
    ">9i", 0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000
)


class Mp4BuildError(ValueError):
    pass


def _video_entry(track: NormalizedTrack) -> bytes:
    sample = track.track
    compressor = sample.name.encode("utf-8")[:31].ljust(31, b"\x00") + b"\x00"
    payload = (
        b"\x00" * 6
        + u16(1)
        + b"\x00" * 16
        + u16(sample.width or 1920)
        + u16(sample.height or 1080)
        + u32(0x00480000)
        + u32(0x00480000)
        + b"\x00" * 4
        + u16(1)
        + compressor
        + u16(0x0018)
        + struct.pack(">h", -1)
    )
    config_type = {"avc1": "avcC", "hvc1": "hvcC", "av01": "av1C"}.get(sample.mp4_codec)
    if config_type is not None:
        payload += box(config_type, sample.codec_private)
    return box(sample.mp4_codec, payload)


def _esds(codec_private: bytes, sample_rate: float, channels: int) -> bytes:
    decoder_config = (
        bytes([0x40, 0x15])
        + b"\x00\x00\x00"
        + u32(0)
        + u32(128_000)
        + descriptor(5, codec_private)
    )
    es = u16(1) + b"\x00" + descriptor(4, decoder_config) + descriptor(6, b"\x02")
    return full_box("esds", 0, 0, descriptor(3, es))


def _audio_entry(track: NormalizedTrack) -> bytes:
    sample = track.track
    channels = sample.channels or 2
    rate = int(round(sample.sample_rate or 48000))
    payload = (
        b"\x00" * 6
        + u16(1)
        + u16(0)
        + u16(0)
        + u32(0)
        + u16(channels)
        + u16(16)
        + u16(0)
        + u16(0)
        + u32(rate << 16)
    )
    if sample.mp4_codec == "mp4a":
        payload += _esds(sample.codec_private, sample.sample_rate, channels)
    return box(sample.mp4_codec, payload)


def build_stsd(track: NormalizedTrack) -> bytes:
    if track.track.track_type == TRACK_TYPE_AUDIO:
        entry = _audio_entry(track)
    else:
        entry = _video_entry(track)
    return full_box("stsd", 0, 0, u32(1) + entry)


def _mdhd(track: NormalizedTrack) -> bytes:
    duration = track.last_dts + int(track.durations[-1])
    lang = 0x55C4
    payload = u32(0) + u32(0) + u32(track.track.timescale) + u32(duration)
    payload += u16(lang) + u16(0)
    return full_box("mdhd", 0, 0, payload)


def _hdlr(audio: bool) -> bytes:
    name = b"SoundHandler\x00" if audio else b"VideoHandler\x00"
    return full_box("hdlr", 0, 0, u32(0) + b"soun" * audio + b"vide" * (not audio) + b"\x00" * 12 + name)


def _dinf() -> bytes:
    url = full_box("url ", 0, 1)
    return container("dinf", full_box("dref", 0, 0, u32(1) + url))


def _minf(track: NormalizedTrack, stbl: bytes) -> bytes:
    audio = track.track.track_type == TRACK_TYPE_AUDIO
    media_header = full_box("smhd", 0, 0, b"\x00" * 4) if audio else full_box("vmhd", 0, 1, b"\x00" * 8)
    return container("minf", media_header, _dinf(), stbl)


def _tkhd(track: NormalizedTrack) -> bytes:
    audio = track.track.track_type == TRACK_TYPE_AUDIO
    duration = track.last_dts + int(track.durations[-1])
    payload = (
        u32(0) + u32(0) + u32(track.track.mp4_track_id) + u32(0) + u32(duration)
        + b"\x00" * 8 + u16(0) + u16(0)
        + u16(0x0100 if audio else 0) + u16(0) + MATRIX
        + u32((track.track.width or 0) << 16)
        + u32((track.track.height or 0) << 16)
    )
    return full_box("tkhd", 0, 0x000007, payload)


def build_trak(track: NormalizedTrack, offsets: list[int], *, fragmented: bool) -> bytes:
    stsd = build_stsd(track)
    stbl = stbl_empty(stsd) if fragmented else stbl_plain(track, offsets, stsd)
    audio = track.track.track_type == TRACK_TYPE_AUDIO
    children = [_tkhd(track)]
    if not fragmented and track.first_dts > 0:
        edit_duration = int(round(track.first_dts * 1000 / track.track.timescale))
        elst = full_box("elst", 0, 0, u32(1) + u32(edit_duration) + struct.pack(">iI", -1, 0x00010000))
        children.append(container("edts", elst))
    children.append(container("mdia", _mdhd(track), _hdlr(audio), _minf(track, stbl)))
    return container("trak", *children)


def build_mvhd(media: NormalizedMedia, timescale: int = 1000) -> bytes:
    durations = [t.last_dts + int(t.durations[-1]) for t in media.tracks]
    duration = max(durations, default=0)
    payload = (
        u32(0) + u32(0) + u32(timescale) + u32(duration)
        + u32(0x00010000) + u16(0x0100) + u16(0)
        + b"\x00" * 8 + MATRIX + b"\x00" * 24 + u32(len(media.tracks) + 1)
    )
    return full_box("mvhd", 0, 0, payload)


def build_mvex(media: NormalizedMedia) -> bytes:
    trex = []
    for track in media.tracks:
        payload = u32(track.track.mp4_track_id) + u32(1) + u32(0) + u32(0) + u32(0)
        trex.append(full_box("trex", 0, 0, payload))
    return container("mvex", *trex)


def build_moov(
    media: NormalizedMedia,
    offsets: dict[int, list[int]],
    *,
    fragmented: bool,
) -> bytes:
    children = [build_mvhd(media)]
    for track in media.tracks:
        children.append(
            build_trak(track, offsets.get(track.track.mkv_track, []), fragmented=fragmented)
        )
    if fragmented:
        children.append(build_mvex(media))
    return container("moov", *children)
