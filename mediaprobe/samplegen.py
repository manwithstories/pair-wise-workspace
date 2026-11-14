from __future__ import annotations

import hashlib
import os
import struct
from dataclasses import dataclass, field
from pathlib import Path


MATRIX = struct.pack(">9I", 0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000)


def box(box_type: str, payload: bytes = b"", *, large: bool = False, zero: bool = False) -> bytes:
    kind = box_type.encode("latin-1")
    if zero:
        return struct.pack(">I", 0) + kind + payload
    if large:
        return struct.pack(">I", 1) + kind + struct.pack(">Q", 16 + len(payload)) + payload
    return struct.pack(">I", 8 + len(payload)) + kind + payload


def fullbox(version: int, flags: int, payload: bytes = b"") -> bytes:
    return struct.pack(">I", (version << 24) | flags) + payload


def deterministic_bytes(label: str, size: int) -> bytes:
    output = bytearray()
    counter = 0
    while len(output) < size:
        output.extend(hashlib.sha256(f"{label}:{counter}".encode("utf-8")).digest())
        counter += 1
    return bytes(output[:size])


def _language_code(language: str | None) -> int:
    if not language:
        return 0x55C4
    chars = {"en": "eng", "zh": "zho", "ja": "jpn"}.get(language.lower(), (language.lower() + "und")[:3])
    return ((ord(chars[0]) - 0x60) << 10) | ((ord(chars[1]) - 0x60) << 5) | (ord(chars[2]) - 0x60)


@dataclass
class TrackSpec:
    track_id: int
    handler: str
    codec: str
    language: str | None = None
    duration_ms: int = 12000
    timescale: int = 1000
    profile: str | None = None
    level: str | None = None
    sample_rate: int = 48000
    channels: int = 2
    width: int = 1920
    height: int = 1080
    sample_count: int = 1
    sample_size: int = 4096
    stsd_declared: int | None = None
    bad_chunk_offset: bool = False
    use_elst: bool = False
    payload: bytes = field(default=b"", repr=False)

    @property
    def delta_units(self) -> int:
        return max(1, self.duration_ms * self.timescale // (1000 * self.sample_count))


def _video_entry(track: TrackSpec) -> bytes:
    profile_idc = {"High": 100, "Main": 77, "Baseline": 66}.get(track.profile or "", 100)
    level_idc = int(float(track.level or "4.0") * 10)
    avcc = box("avcC", bytes([1, profile_idc, 0, level_idc, 0xFF, 0xE1, 0, 0, 0, 0x01, 0x00, 0x00]))
    payload = (
        b"\x00" * 6
        + struct.pack(">H", 1)
        + b"\x00" * 16
        + struct.pack(">HH", track.width, track.height)
        + struct.pack(">II", 0x00480000, 0x00480000)
        + struct.pack(">I", 0)
        + struct.pack(">H", 1)
        + b"\x00" * 32
        + struct.pack(">Hh", 0x0018, -1)
        + avcc
    )
    return box("avc1", payload)


def _descriptor(tag: int, payload: bytes) -> bytes:
    if len(payload) > 127:
        raise ValueError("fixture descriptor is too large")
    return bytes([tag, len(payload)]) + payload


def _audio_entry(track: TrackSpec) -> bytes:
    asc = bytes([(2 << 3) | (3 >> 1), ((3 & 1) << 7) | (track.channels << 3)])
    decoder_config = bytes([0x40, 0x05, 0, 0, 0]) + struct.pack(">III", 0, 0, 0) + _descriptor(0x05, asc)
    es_descriptor = b"\x00\x01\x00" + _descriptor(0x04, decoder_config) + _descriptor(0x06, b"\x02")
    esds = box("esds", fullbox(0, 0) + _descriptor(0x03, es_descriptor))
    payload = (
        b"\x00" * 6
        + struct.pack(">H", 1)
        + struct.pack(">HH", 0, 0)
        + struct.pack(">I", 0)
        + struct.pack(">HH", track.channels, 16)
        + struct.pack(">HH", 0, 0)
        + struct.pack(">I", track.sample_rate << 16)
        + esds
    )
    return box("mp4a", payload)


def _sample_entry(track: TrackSpec) -> tuple[str, bytes]:
    if track.handler == "video":
        return "avc1", _video_entry(track)
    if track.handler == "audio":
        return "mp4a", _audio_entry(track)
    if track.handler == "timecode":
        return "tmcd", box("tmcd", b"\x00" * 16)
    if track.handler == "subtitle":
        return "tx3g", box("tx3g", b"\x00" * 8)
    raise ValueError(f"unsupported fixture handler: {track.handler}")


def _stsd(track: TrackSpec) -> bytes:
    entry_type, entry = _sample_entry(track)
    declared = track.stsd_declared if track.stsd_declared is not None else 1
    del entry_type
    return box("stsd", fullbox(0, 0) + struct.pack(">I", declared) + entry)


def _mvhd(timescale: int, duration: int) -> bytes:
    payload = (
        b"\x00" * 8
        + struct.pack(">II", timescale, duration)
        + struct.pack(">I", 0x00010000)
        + struct.pack(">H", 0x0100)
        + b"\x00" * 10
        + MATRIX
        + b"\x00" * 24
        + struct.pack(">I", 2)
    )
    return box("mvhd", fullbox(0, 0, payload))


def _tkhd(track: TrackSpec, movie_duration: int) -> bytes:
    volume = 0x0100 if track.handler == "audio" else 0
    payload = (
        b"\x00" * 8
        + struct.pack(">I", track.track_id)
        + b"\x00" * 4
        + struct.pack(">I", movie_duration)
        + b"\x00" * 8
        + struct.pack(">hhhH", 0, 0, volume, 0)
        + MATRIX
        + struct.pack(">II", track.width << 16, track.height << 16)
    )
    return box("tkhd", fullbox(0, 7, payload))


def _mdhd(track: TrackSpec) -> bytes:
    sample_duration = track.duration_ms * track.timescale // 1000
    payload = (
        b"\x00" * 8
        + struct.pack(">II", track.timescale, sample_duration)
        + struct.pack(">HH", _language_code(track.language), 0)
    )
    return box("mdhd", fullbox(0, 0, payload))


def _hdlr(track: TrackSpec) -> bytes:
    handler_type = {"video": "vide", "audio": "soun", "timecode": "tmcd", "subtitle": "sbtl"}[track.handler]
    name = (track.handler + "\x00").encode("utf-8")
    return box("hdlr", fullbox(0, 0, b"\x00" * 4 + handler_type.encode("latin-1") + b"\x00" * 12 + name))


def _media_header(track: TrackSpec) -> bytes:
    if track.handler == "video":
        return box("vmhd", fullbox(0, 1, b"\x00" * 8))
    if track.handler == "audio":
        return box("smhd", fullbox(0, 0, b"\x00" * 4))
    return box("nmhd", fullbox(0, 0))


def _dinf() -> bytes:
    url = box("url ", fullbox(0, 1))
    return box("dinf", box("dref", fullbox(0, 0) + struct.pack(">I", 1) + url))


def _stbl(track: TrackSpec, chunk_offset: int, *, empty: bool = False) -> bytes:
    stsd = _stsd(track)
    if empty:
        stts = box("stts", fullbox(0, 0) + struct.pack(">I", 0))
        stsc = box("stsc", fullbox(0, 0) + struct.pack(">I", 0))
        stsz = box("stsz", fullbox(0, 0) + struct.pack(">II", 0, 0))
        stco = box("stco", fullbox(0, 0) + struct.pack(">I", 0))
    else:
        stts = box(
            "stts",
            fullbox(0, 0)
            + struct.pack(">I", 1)
            + struct.pack(">II", track.sample_count, track.delta_units),
        )
        stsc = box(
            "stsc",
            fullbox(0, 0)
            + struct.pack(">I", 1)
            + struct.pack(">III", 1, track.sample_count, 1),
        )
        sizes = [track.sample_size] * track.sample_count
        stsz = box(
            "stsz",
            fullbox(0, 0)
            + struct.pack(">II", 0, track.sample_count)
            + b"".join(struct.pack(">I", size) for size in sizes),
        )
        chunk = 999_999_999 if track.bad_chunk_offset else chunk_offset
        stco = box("stco", fullbox(0, 0) + struct.pack(">I", 1) + struct.pack(">I", chunk))
    return box("stbl", stsd + stts + stsc + stsz + stco)


def _elst(track: TrackSpec) -> bytes:
    return box(
        "edts",
        box(
            "elst",
            fullbox(0, 0)
            + struct.pack(">I", 1)
            + struct.pack(">IiHH", track.duration_ms, 0, 1, 0),
        ),
    )


def _trak(track: TrackSpec, movie_duration: int, chunk_offset: int, *, empty: bool = False) -> bytes:
    minf = box(
        "minf",
        _media_header(track)
        + _dinf()
        + _stbl(track, chunk_offset, empty=empty),
    )
    mdia = box("mdia", _mdhd(track) + _hdlr(track) + minf)
    edts = _elst(track) if track.use_elst else b""
    return box("trak", _tkhd(track, movie_duration) + edts + mdia)


def _trex(track: TrackSpec) -> bytes:
    return box(
        "trex",
        fullbox(0, 0)
        + struct.pack(">IIIII", track.track_id, 1, track.delta_units, track.sample_size, 0),
    )


def _moov(
    tracks: list[TrackSpec],
    offsets: dict[int, int],
    movie_duration: int,
    *,
    empty: bool = False,
) -> bytes:
    children = [_mvhd(1000, movie_duration)]
    for track in tracks:
        children.append(_trak(track, movie_duration, offsets.get(track.track_id, 0), empty=empty))
    if empty:
        children.append(box("mvex", b"".join(_trex(track) for track in tracks)))
    return box("moov", b"".join(children))


def _ftyp(mov: bool = False) -> bytes:
    if mov:
        return box("ftyp", b"qt  " + struct.pack(">I", 0x200) + b"qt  ")
    return box("ftyp", b"isom" + struct.pack(">I", 0x200) + b"isomiso2avc1mp41")


def _assign_payload(tracks: list[TrackSpec], seed: int, label: str) -> None:
    for track in tracks:
        if not track.payload:
            size = track.sample_size * track.sample_count
            track.payload = deterministic_bytes(f"{seed}:{label}:{track.track_id}:{track.handler}", size)


def _uuid_box() -> bytes:
    return box("uuid", b"mediaprobe-uuid0" + b"metadata")


def _regular_mp4(
    tracks: list[TrackSpec],
    seed: int,
    label: str,
    *,
    mov: bool = False,
    large_mdat: bool = False,
    trailing_zero: bool = False,
    include_uuid: bool = False,
) -> bytes:
    _assign_payload(tracks, seed, label)
    prefix = _ftyp(mov) + (_uuid_box() if include_uuid else b"")
    mdat_payload = b"".join(track.payload for track in tracks)
    mdat_header = 16 if large_mdat else 8
    data_start = len(prefix) + mdat_header
    offsets: dict[int, int] = {}
    cursor = data_start
    for track in tracks:
        offsets[track.track_id] = cursor
        cursor += len(track.payload)
    movie_duration = max(track.duration_ms for track in tracks)
    mdat = box("mdat", mdat_payload, large=large_mdat)
    moov = _moov(tracks, offsets, movie_duration)
    tail = box("free", zero=True) if trailing_zero else b""
    return prefix + mdat + moov + tail


def _fragment_mp4(tracks: list[TrackSpec], seed: int, label: str) -> bytes:
    _assign_payload(tracks, seed, label)
    prefix = _ftyp()
    moov = _moov(tracks, {}, max(track.duration_ms for track in tracks), empty=True)
    relative: dict[int, int] = {}
    cursor = 0
    for track in tracks:
        relative[track.track_id] = cursor
        cursor += len(track.payload)
    mdat_payload = b"".join(track.payload for track in tracks)
    mdat = box("mdat", mdat_payload)

    def make_moof(data_offsets: dict[int, int]) -> bytes:
        trafs = []
        for track in tracks:
            tfhd = box(
                "tfhd",
                fullbox(0, 0x18)
                + struct.pack(">I", track.track_id)
                + struct.pack(">II", track.delta_units, track.sample_size),
            )
            tfdt = box("tfdt", fullbox(1, 0) + struct.pack(">Q", 0))
            trun = box(
                "trun",
                fullbox(0, 0x000001)
                + struct.pack(">I", track.sample_count)
                + struct.pack(">i", data_offsets[track.track_id]),
            )
            trafs.append(box("traf", tfhd + tfdt + trun))
        mfhd = box("mfhd", fullbox(0, 0) + struct.pack(">I", 1))
        return box("moof", mfhd + b"".join(trafs))

    zero_moof = make_moof({track.track_id: 0 for track in tracks})
    offsets = {
        track.track_id: len(zero_moof) + 8 + relative[track.track_id]
        for track in tracks
    }
    return prefix + moov + make_moof(offsets) + mdat


def _riff_chunk(chunk_id: bytes, payload: bytes) -> bytes:
    padding = b"\x00" if len(payload) & 1 else b""
    return chunk_id + struct.pack("<I", len(payload)) + payload + padding


WAVE_EXTENSIBLE_GUID = struct.pack("<IHH8s", 1, 0, 0x0010, b"\x80\x00\x00\xaa\x00\x38\x9b\x71")


def _wav(
    seed: int,
    label: str,
    *,
    duration_ms: int,
    extensible: bool = False,
    language: str | None = None,
) -> bytes:
    del language
    channels, sample_rate, bits = 2, 48000, 16
    block_align = channels * bits // 8
    byte_rate = sample_rate * block_align
    data_size = byte_rate * duration_ms // 1000
    sample_data = deterministic_bytes(f"{seed}:{label}:wav", data_size)
    if extensible:
        fmt_payload = struct.pack("<HHIIHH", 0xFFFE, channels, sample_rate, byte_rate, block_align, bits)
        fmt_payload += struct.pack("<HHI", 22, bits, 3) + WAVE_EXTENSIBLE_GUID
    else:
        fmt_payload = struct.pack("<HHIIHH", 1, channels, sample_rate, byte_rate, block_align, bits)
    chunks = (
        _riff_chunk(b"JUNK", b"odd")
        + _riff_chunk(b"fmt ", fmt_payload)
        + _riff_chunk(b"LIST", b"mediaprobe")
        + _riff_chunk(b"data", sample_data)
    )
    return b"RIFF" + struct.pack("<I", 4 + len(chunks)) + b"WAVE" + chunks


def _video(track_id: int, *, profile: str = "High", level: str = "4.0", language: str | None = None) -> TrackSpec:
    return TrackSpec(
        track_id=track_id,
        handler="video",
        codec="h264",
        profile=profile,
        level=level,
        language=language,
        timescale=90000,
        sample_size=8192,
    )


def _audio(track_id: int, language: str | None, *, duration_ms: int = 12000) -> TrackSpec:
    return TrackSpec(
        track_id=track_id,
        handler="audio",
        codec="aac",
        language=language,
        duration_ms=duration_ms,
        timescale=48000,
        width=0,
        height=0,
        sample_size=4096,
    )


def _timecode(track_id: int) -> TrackSpec:
    return TrackSpec(
        track_id=track_id,
        handler="timecode",
        codec="tmcd",
        duration_ms=12000,
        timescale=1000,
        width=0,
        height=0,
        sample_size=1024,
    )


def sample_recipes(seed: int) -> list[tuple[str, bytes]]:
    """Build the deterministic 14-asset acceptance corpus."""

    recipes: list[tuple[str, bytes]] = []
    recipes.append(
        (
            "trailer_1080_av.mp4",
            _regular_mp4(
                [_video(1), _audio(2, "en")],
                seed,
                "trailer_1080_av",
                large_mdat=True,
                trailing_zero=True,
                include_uuid=True,
            ),
        )
    )
    recipes.append(
        (
            "trailer_1080_multilang.mp4",
            _regular_mp4([_video(1), _audio(2, "en"), _audio(3, "zh")], seed, "trailer_multilang"),
        )
    )
    timecode_video = _video(1)
    timecode_video.use_elst = True
    recipes.append(
        (
            "trailer_1080_timecode.mov",
            _regular_mp4([timecode_video, _audio(2, "en"), _timecode(3)], seed, "trailer_timecode", mov=True),
        )
    )
    recipes.append(
        (
            "trailer_fragmented.mp4",
            _fragment_mp4(
                [
                    TrackSpec(1, "video", "h264", profile="High", level="4.0", timescale=90000, sample_count=2, sample_size=2048),
                    TrackSpec(2, "audio", "aac", language="en", timescale=48000, sample_count=2, sample_size=1536),
                ],
                seed,
                "trailer_fragmented",
            ),
        )
    )
    recipes.append(
        (
            "gameplay_1080_audio.mp4",
            _regular_mp4([_video(1), _audio(2, "en")], seed, "gameplay_audio"),
        )
    )
    recipes.append(
        (
            "gameplay_1080_video.mp4",
            _regular_mp4([_video(1), _audio(2, "zh")], seed, "gameplay_video"),
        )
    )
    recipes.append(
        (
            "trailer_1080_mobile.mp4",
            _regular_mp4([_video(1), _audio(2, "en")], seed, "trailer_mobile"),
        )
    )
    recipes.append(("voice_en.wav", _wav(seed, "voice_en", duration_ms=4000)))
    recipes.append(("voice_zh.wav", _wav(seed, "voice_zh", duration_ms=4000)))
    recipes.append(
        ("sfx_ambience.wav", _wav(seed, "sfx_ambience", duration_ms=3000, extensible=True))
    )
    recipes.append(("sfx_ui.wav", _wav(seed, "sfx_ui", duration_ms=500)))
    recipes.append(
        (
            "trailer_audio_short.mp4",
            _regular_mp4([_video(1), _audio(2, "en", duration_ms=11952)], seed, "trailer_audio_short"),
        )
    )
    conflict_audio = _audio(3, "zh")
    conflict_audio.stsd_declared = 2
    recipes.append(
        (
            "trailer_dual_audio_conflict.mp4",
            _regular_mp4(
                [_video(1), _audio(2, "en"), conflict_audio, _audio(2, "ja")],
                seed,
                "trailer_dual_audio_conflict",
            ),
        )
    )
    compat_video = _video(1, profile="Main", level="3.1")
    compat_video.bad_chunk_offset = True
    recipes.append(
        (
            "trailer_mobile_compat.mp4",
            _regular_mp4([compat_video, _audio(2, "en")], seed, "trailer_mobile_compat"),
        )
    )
    return recipes


def generate_samples(out: str | os.PathLike[str], count: int, seed: int) -> list[str]:
    if count < 0:
        raise ValueError("count must be non-negative")
    destination = Path(out)
    destination.mkdir(parents=True, exist_ok=True)
    base = sample_recipes(seed)
    selected = base[:count]
    for index in range(count - len(base)):
        name = f"extra_{index + 1:03d}.wav"
        selected.append((name, _wav(seed, f"extra_{index + 1}", duration_ms=1000)))
    paths = []
    for name, payload in selected:
        target = destination / name
        temporary = target.with_suffix(target.suffix + ".tmp")
        temporary.write_bytes(payload)
        os.replace(temporary, target)
        paths.append(os.fspath(target))
    return paths
