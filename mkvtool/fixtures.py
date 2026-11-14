"""Deterministic Matroska fixtures generated without external muxers."""

from __future__ import annotations

import json
import math
import os
from bisect import bisect_left
from dataclasses import dataclass, field

from mkvtool.ebml.vint import encode_element_id, encode_signed, encode_unknown_size, encode_vint

TIMECODE_SCALE_NS = 1_000_000
VIDEO_DURATION_MS = 40
AUDIO_DURATION_MS = 21


def element(element_id: int, payload: bytes, *, unknown_size: bool = False) -> bytes:
    size = encode_unknown_size() if unknown_size else encode_vint(len(payload))
    return encode_element_id(element_id) + size + payload


def uint_element(element_id: int, value: int) -> bytes:
    width = max(1, (int(value).bit_length() + 7) // 8)
    return element(element_id, int(value).to_bytes(width, "big"))


def int_element(element_id: int, value: int, width: int = 2) -> bytes:
    return element(element_id, encode_signed(value, width))


def string_element(element_id: int, value: str) -> bytes:
    return element(element_id, value.encode("utf-8"))


def float_element(element_id: int, value: float) -> bytes:
    import struct

    return element(element_id, struct.pack(">d", value))


def _simple_block(track_number: int, relative_ms: int, keyframe: bool, payload: bytes) -> bytes:
    body = (
        encode_vint(track_number) + encode_signed(relative_ms, 2)
        + bytes([0x80 if keyframe else 0x00]) + payload
    )
    return element(0xA3, body)


def _block_group(
    track_number: int,
    relative_ms: int,
    keyframe: bool,
    payload: bytes,
    duration_ms: int,
    composition_offset_ms: int = 0,
) -> bytes:
    body = encode_vint(track_number) + encode_signed(relative_ms, 2) + b"\x00" + payload
    children = [
        element(0xA1, body),
        uint_element(0x9B, duration_ms),
    ]
    if not keyframe:
        children.append(int_element(0xFB, -duration_ms))
    if composition_offset_ms:
        addition = b"PTSOFF:" + str(composition_offset_ms).encode("ascii")
        block_more = element(0xEE, b"\x01") + element(0xA5, addition)
        children.append(element(0x75A1, element(0xA6, block_more)))
    return element(0xA0, b"".join(children))


def _avcc() -> bytes:
    sps = b"\x64\x00\x1f\xac\xd9\x40\x50\x04\x00\x00\x03\x00\x02\x00\x00\x03\x00\x64\x1e\x2c\x5c\x90"
    pps = b"\x68\xeb\xe3\xcb\x22\xc0"
    return (
        b"\x01\x64\x00\x1f\xff\xe1" + len(sps).to_bytes(2, "big") + sps
        + b"\x01" + len(pps).to_bytes(2, "big") + pps
    )


def _header() -> bytes:
    payload = b"".join([
        uint_element(0x4286, 1),
        uint_element(0x42F7, 1),
        uint_element(0x42F2, 4),
        uint_element(0x42F3, 8),
        string_element(0x4282, "matroska"),
        uint_element(0x4287, 4),
        uint_element(0x4285, 2),
    ])
    return element(0x1A45DFA3, payload)


def _tracks(extra_unsupported: bool = False) -> bytes:
    video_codec = "V_MPEG2" if extra_unsupported == "all" else "V_MPEG4/ISO/AVC"
    audio_codec = "A_OPUS" if extra_unsupported == "all" else "A_AAC"
    video = b"".join([
        uint_element(0xD7, 1),
        uint_element(0x73C5, 1),
        uint_element(0x83, 1),
        uint_element(0x88, 1),
        uint_element(0x9C, 0),
        string_element(0x86, video_codec),
        element(0x63A2, _avcc()),
        uint_element(0x23E383, VIDEO_DURATION_MS * 1_000_000),
        string_element(0x22B59C, "und"),
        element(0xE0, uint_element(0xB0, 640) + uint_element(0xBA, 360)
                + element(0x55B0, uint_element(0x55B1, 1))),
    ])
    audio = b"".join([
        uint_element(0xD7, 2),
        uint_element(0x73C5, 2),
        uint_element(0x83, 2),
        uint_element(0x88, 1),
        uint_element(0x9C, 0),
        string_element(0x86, audio_codec),
        element(0x63A2, b"\x11\x90"),
        uint_element(0x23E383, AUDIO_DURATION_MS * 1_000_000),
        string_element(0x22B59C, "und"),
        element(0xE1, float_element(0xB5, 48000.0) + uint_element(0x9F, 2)),
    ])
    entries = [
        element(0xAE, video),
        element(0xAE, audio),
    ]
    if extra_unsupported is True:
        entries.append(element(0xAE, b"".join([
            uint_element(0xD7, 3),
            uint_element(0x73C5, 3),
            uint_element(0x83, 17),
            uint_element(0x88, 0),
            string_element(0x86, "S_TEXT/UTF8"),
            string_element(0x22B59C, "chi"),
        ])))
    return element(0x1654AE6B, b"".join(entries))


def _tags() -> bytes:
    nested = element(0x67C8, string_element(0x45A3, "NEST2") + string_element(0x4487, "deep"))
    simple = element(0x67C8, string_element(0x45A3, "NEST1") + nested)
    tag = element(0x7373, element(0x63C0, uint_element(0x68CA, 50)) + simple)
    return element(0x1254C367, tag)


@dataclass
class FixtureOptions:
    name: str
    video_frames: int = 1500
    audio_frames: int = 1500
    unknown_segment: bool = False
    unknown_clusters: int = 0
    negative_dts: bool = False
    b_frames: bool = False
    gap_after: int | None = None
    gap_ms: int = 3000
    cues: str = "full"
    extra_unsupported: bool = False
    all_unsupported: bool = False
    truncate: bool = False
    corrupt_header: bool = False
    cluster_count: int = 8
    block_group_every: int = 5
    metadata: dict = field(default_factory=dict)


@dataclass
class Fixture:
    path: str
    data: bytes
    options: FixtureOptions
    truncation_offset: int | None = None


@dataclass
class _SampleSpec:
    number: int
    index: int
    dts_ms: int
    duration_ms: int
    keyframe: bool
    payload: bytes
    composition_ms: int = 0
    use_group: bool = False


def _video_samples(options: FixtureOptions) -> list[_SampleSpec]:
    samples: list[_SampleSpec] = []
    offsets = (80, 120, 40)
    for index in range(options.video_frames):
        dts = index * VIDEO_DURATION_MS
        if options.negative_dts:
            dts -= 80
        if options.gap_after is not None and index >= options.gap_after:
            dts += options.gap_ms
        keyframe = index % 40 == 0
        payload = (
            bytes([0x65 if keyframe else 0x41])
            + index.to_bytes(4, "big", signed=True)
            + (b"\x11" * (40 if keyframe else 27))
        )
        samples.append(_SampleSpec(
            number=1,
            index=index,
            dts_ms=dts,
            duration_ms=VIDEO_DURATION_MS,
            keyframe=keyframe,
            payload=payload,
            composition_ms=offsets[index % 3] if options.b_frames else 0,
            use_group=(
                options.b_frames or index % options.block_group_every == 0
            ) and options.block_group_every > 0,
        ))
    return samples


def _audio_samples(count: int) -> list[_SampleSpec]:
    return [
        _SampleSpec(
            number=2,
            index=index,
            dts_ms=index * AUDIO_DURATION_MS,
            duration_ms=AUDIO_DURATION_MS,
            keyframe=True,
            payload=b"\x21" + index.to_bytes(4, "big") + b"\x55" * 8,
        )
        for index in range(count)
    ]


def _cue_bytes(
    cue_records: list[dict], mode: str
) -> bytes:
    if mode == "none" or not cue_records:
        return b""
    records = cue_records if mode == "full" else cue_records[: math.ceil(len(cue_records) / 2)]
    points: list[bytes] = []
    for index, record in enumerate(records):
        position = record["cluster_position"]
        if mode == "partial" and index % 2:
            position += 7
        positions = b"".join([
            uint_element(0xF7, 1),
            uint_element(0xF1, position),
            uint_element(0xF0, record["relative_position"]),
            uint_element(0x5378, record["block_number"]),
            uint_element(0xB2, record["duration_ms"]),
        ])
        point = uint_element(0xB3, record["time_ms"]) + element(0xB7, positions)
        points.append(element(0xBB, point))
    return element(0x1C53BB6B, b"".join(points))


def _cluster_bytes(
    cluster_index: int,
    video: list[_SampleSpec],
    audio: list[_SampleSpec],
    start: int,
    end: int,
    unknown: bool,
) -> tuple[bytes, dict[int, dict]]:
    video_part = video[start:end]
    start_ms = video_part[0].dts_ms
    end_ms = video[end].dts_ms if end < len(video) else video_part[-1].dts_ms + video_part[-1].duration_ms
    audio_times = [item.dts_ms for item in audio]
    audio_start = bisect_left(audio_times, start_ms)
    audio_end = bisect_left(audio_times, end_ms)
    audio_part = audio[audio_start:audio_end]
    candidates = [item.dts_ms for item in video_part + audio_part if item.dts_ms >= 0]
    cluster_time = min(candidates) if candidates else 0
    body = uint_element(0xE7, cluster_time)
    cues: dict[int, dict] = {}
    for item in video_part:
        relative = item.dts_ms - cluster_time
        block_offset = len(body)
        if item.use_group:
            block = _block_group(
                item.number, relative, item.keyframe, item.payload,
                item.duration_ms, item.composition_ms,
            )
        else:
            block = _simple_block(item.number, relative, item.keyframe, item.payload)
        body += block
        if item.keyframe:
            cues[item.index] = {
                "cluster_index": cluster_index,
                "relative_position": block_offset,
                "block_number": item.index - start + 1,
                "time_ms": max(0, item.dts_ms + item.composition_ms),
                "duration_ms": item.duration_ms,
            }
    for item in audio_part:
        body += _simple_block(item.number, item.dts_ms - cluster_time, True, item.payload)
    return element(0x1F43B675, body, unknown_size=unknown), cues


def _segment_payload(options: FixtureOptions) -> tuple[bytes, dict]:
    video = _video_samples(options)
    audio = _audio_samples(options.audio_frames)
    info = element(0x1549A966, b"".join([
        uint_element(0x2AD7B1, TIMECODE_SCALE_NS),
        float_element(0x4489, (video[-1].dts_ms + video[-1].duration_ms) if video else 0.0),
        string_element(0x4D80, "mkvtool fixture generator"),
        string_element(0x5741, "mkvtool fixture generator"),
    ]))
    track_variant = "all" if options.all_unsupported else options.extra_unsupported
    payload = info + _tracks(track_variant)
    cluster_count = min(options.cluster_count, max(1, len(video)))
    chunk = math.ceil(len(video) / cluster_count)
    cluster_positions: dict[int, int] = {}
    cue_by_frame: dict[int, dict] = {}
    for cluster_index in range(cluster_count):
        start = cluster_index * chunk
        end = min(len(video), start + chunk)
        if start >= end:
            break
        cluster_positions[cluster_index] = len(payload)
        unknown = cluster_index < options.unknown_clusters
        cluster, cues = _cluster_bytes(
            cluster_index, video, audio, start, end, unknown
        )
        payload += cluster
        for frame_index, record in cues.items():
            record["cluster_position"] = cluster_positions[cluster_index]
            cue_by_frame[frame_index] = record
    cues = _cue_bytes(list(cue_by_frame.values()), options.cues)
    payload += cues + _tags()
    metadata = {
        "cluster_positions": cluster_positions,
        "video_frames": len(video),
        "audio_frames": len(audio),
        "keyframes": sum(1 for sample in video if sample.keyframe),
        "segment_payload_size": len(payload),
    }
    return payload, metadata


def build_fixture(options: FixtureOptions) -> Fixture:
    if options.corrupt_header:
        data = b"\xde\xad\xbe\xef" + b"\x00" * 64
        return Fixture(options.name, data, options)
    payload, metadata = _segment_payload(options)
    options.metadata.update(metadata)
    segment = element(0x18538067, payload, unknown_size=options.unknown_segment)
    data = _header() + segment
    truncation_offset = None
    if options.truncate:
        positions = list(metadata["cluster_positions"].values())
        if positions:
            size_width = 1 if options.unknown_segment else len(encode_vint(len(payload)))
            segment_data_offset = len(_header()) + 4 + size_width
            truncation_offset = segment_data_offset + positions[-1] + 20
            data = data[:truncation_offset]
    return Fixture(options.name, data, options, truncation_offset)


def fixture_catalog() -> list[FixtureOptions]:
    return [
        FixtureOptions(
            "unknown_segment.mkv", unknown_segment=True, unknown_clusters=8,
        ),
        FixtureOptions(
            "negative_dts_b.mkv", unknown_clusters=4, negative_dts=True, b_frames=True,
        ),
        FixtureOptions(
            "no_cues.mkv", video_frames=300, audio_frames=300, cluster_count=3,
            cues="none",
        ),
        FixtureOptions(
            "partial_cues.mkv", video_frames=300, audio_frames=300, cluster_count=3,
            cues="partial", unknown_segment=True, unknown_clusters=3,
        ),
        FixtureOptions(
            "gapped_timeline.mkv", video_frames=120, audio_frames=120,
            gap_after=40, gap_ms=3000, cluster_count=4,
            extra_unsupported=True,
        ),
        FixtureOptions(
            "truncated_cluster.mkv", video_frames=300, audio_frames=300,
            cluster_count=3, unknown_segment=True, unknown_clusters=3, truncate=True,
        ),
        FixtureOptions("corrupt_header.mkv", corrupt_header=True),
    ]


def generate_fixtures(output_dir: str, cases: int = 7) -> list[Fixture]:
    catalog = fixture_catalog()[:max(0, cases)]
    os.makedirs(output_dir, exist_ok=True)
    generated: list[Fixture] = []
    for options in catalog:
        fixture = build_fixture(options)
        path = os.path.join(output_dir, options.name)
        with open(path, "wb") as handle:
            handle.write(fixture.data)
        fixture.path = path
        generated.append(fixture)
    manifest = {
        "cases": len(generated),
        "fixtures": [
            {
                "name": item.options.name,
                "size": len(item.data),
                "truncated": item.truncation_offset is not None,
                **item.options.metadata,
            }
            for item in generated
        ],
    }
    with open(os.path.join(output_dir, "manifest.json"), "w", encoding="utf-8") as handle:
        json.dump(manifest, handle, indent=2, ensure_ascii=False)
    return generated
