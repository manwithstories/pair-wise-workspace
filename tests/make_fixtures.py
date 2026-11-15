#!/usr/bin/env python3
"""Generate synthetic Matroska fixtures with a pure-Python EBML writer.

Fixtures cover: unknown-length Segment/Cluster, missing Cues, B-frames,
Xiph/EBML/fixed lacing, subtitle tracks, truncated inputs, unsupported
codecs and broken timestamps.  No external files or tools are used.
"""
from __future__ import annotations

import argparse
import random
import struct
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from mkvkit.ebml import schema
from mkvkit.ebml.vint import encode_id, encode_size, encode_signed, encode_unknown

# TimestampScale of unknown_len.mkv in nanoseconds (90us base).
SCALE_US90 = 90_000
PTS_TOTAL_UNITS = 111_037  # 111037 * 90000 ns = 9993330000 ns


def el(eid, payload):
    return encode_id(eid) + encode_size(len(payload)) + payload


def master(eid, *children):
    return el(eid, b"".join(children))


def master_unknown(eid, *children, size_len=8):
    return encode_id(eid) + encode_unknown(size_len) + b"".join(children)


def uint_el(eid, value):
    return el(eid, value.to_bytes(max(1, (value.bit_length() + 7) // 8), "big"))


def int_el(eid, value):
    length = 1
    while not (-(1 << (8 * length - 1)) <= value < (1 << (8 * length - 1))):
        length += 1
    return el(eid, value.to_bytes(length, "big", signed=True))


def float_el(eid, value):
    return el(eid, struct.pack(">d", value))


def str_el(eid, text):
    return el(eid, text.encode("ascii"))


def utf8_el(eid, text):
    return el(eid, text.encode("utf-8"))


def xiph_size(n):
    out = bytearray()
    while n >= 255:
        out.append(255)
        n -= 255
    out.append(n)
    return bytes(out)


def block_payload(track, ts, flags, frames, lace=None):
    head = encode_size(track) + struct.pack(">h", ts) + bytes([flags])
    if lace is None:
        return head + frames[0]
    num = len(frames)
    if lace == "xiph":
        lace_head = bytes([num - 1]) + b"".join(
            xiph_size(len(f)) for f in frames[:-1]
        )
    elif lace == "fixed":
        assert len({len(f) for f in frames}) == 1
        lace_head = bytes([num - 1])
    elif lace == "ebml":
        lace_head = bytes([num - 1]) + encode_size(len(frames[0]))
        for prev, cur in zip(frames, frames[1:-1]):
            lace_head += encode_signed(len(cur) - len(prev))
    else:  # pragma: no cover
        raise ValueError(lace)
    return head + lace_head + b"".join(frames)


def block_group(payload, keyframe, duration_units, refs=()):
    children = [
        el(schema.ID_BLOCK, payload),
        uint_el(0xFA, 0),  # ReferencePriority
    ]
    for ref in refs:
        children.append(int_el(schema.ID_REFERENCE_BLOCK, ref))
    children += [
        int_el(0xFD, 0),  # ReferenceVirtual
        uint_el(schema.ID_BLOCK_DURATION, duration_units),
        int_el(0x75A2, 0),  # DiscardPadding
        el(0xA4, b""),  # CodecState
        el(0xA2, b""),  # BlockVirtual
        master(0x8E),  # Slices
        master(0x75A1),  # BlockAdditions
    ]
    return master(schema.ID_BLOCK_GROUP, *children)


def ebml_header():
    return master(
        schema.ID_EBML,
        uint_el(0x4286, 1), uint_el(0x42F7, 1), uint_el(0x42F2, 4),
        uint_el(0x42F3, 8), str_el(schema.ID_DOCTYPE, "matroska"),
        uint_el(0x4287, 4), uint_el(0x4285, 2),
    )


def info(scale, duration_units):
    return master(
        schema.ID_INFO,
        uint_el(schema.ID_TIMESTAMP_SCALE, scale),
        float_el(0x4489, float(duration_units)),
        utf8_el(0x4D80, "mkvkit-fixture"),
        utf8_el(0x5741, "mkvkit-fixture"),
        int_el(0x4461, 0),
    )


def video_track_entry(avcc, number=1):
    return master(
        schema.ID_TRACK_ENTRY,
        uint_el(schema.ID_TRACK_NUMBER, number),
        uint_el(0x73C5, number),
        uint_el(schema.ID_TRACK_TYPE, 1),
        uint_el(0x9C, 0),
        str_el(schema.ID_CODEC_ID, "V_MPEG4/ISO/AVC"),
        el(schema.ID_CODEC_PRIVATE, avcc),
        str_el(schema.ID_LANGUAGE, "und"),
        master(
            schema.ID_VIDEO,
            uint_el(schema.ID_PIXEL_WIDTH, 1920),
            uint_el(schema.ID_PIXEL_HEIGHT, 1080),
            uint_el(0x54B0, 1920),
            uint_el(0x54BA, 1080),
        ),
    )


def audio_track_entry(asc, number=2, rate=48000, channels=2):
    return master(
        schema.ID_TRACK_ENTRY,
        uint_el(schema.ID_TRACK_NUMBER, number),
        uint_el(0x73C5, number),
        uint_el(schema.ID_TRACK_TYPE, 2),
        str_el(schema.ID_CODEC_ID, "A_AAC"),
        el(schema.ID_CODEC_PRIVATE, asc),
        str_el(schema.ID_LANGUAGE, "und"),
        master(
            schema.ID_AUDIO,
            float_el(schema.ID_SAMPLING_FREQUENCY, float(rate)),
            uint_el(schema.ID_CHANNELS, channels),
        ),
    )


def cluster(ts_units, groups, unknown=False):
    children = [uint_el(schema.ID_TIMESTAMP, ts_units), el(schema.ID_CRC32, b"\x00" * 4)]
    children += groups
    if unknown:
        return master_unknown(schema.ID_CLUSTER, *children, size_len=2)
    return master(schema.ID_CLUSTER, *children)


def fake_avcc():
    sps = bytes([0x67, 0x64, 0x00, 0x1F]) + bytes(range(27))
    pps = bytes([0x68, 0xEE, 0x3C, 0x80])
    return b"\x01\x64\x00\x1F\xFF\xE1" + len(sps).to_bytes(2, "big") + sps + b"\x01" + len(pps).to_bytes(2, "big") + pps


def fake_asc():
    return b"\x11\x90"  # AAC-LC, 48 kHz, stereo


def fake_video_frame(rng, keyframe):
    nal_size = rng.randint(900, 1400) if keyframe else rng.randint(200, 700)
    nal = bytes([0x65 if keyframe else 0x41]) + rng.randbytes(nal_size - 1)
    return len(nal).to_bytes(4, "big") + nal


def fake_audio_frame(rng):
    return b"\x21\x10" + rng.randbytes(rng.randint(180, 300))


def _cluster_of(pts, cluster_ts):
    idx = 0
    for i, ts in enumerate(cluster_ts):
        if pts >= ts:
            idx = i
    return idx


def build_unknown_len(rng):
    video_pts = [(i * PTS_TOTAL_UNITS) // 299 for i in range(300)]
    audio_pts = [(i * PTS_TOTAL_UNITS) // 1559 for i in range(1560)]
    audio_ext = audio_pts + [PTS_TOTAL_UNITS + 71]
    cluster_ts = [(c * PTS_TOTAL_UNITS) // 8 for c in range(8)]
    groups = [[] for _ in range(8)]
    for i, pts in enumerate(video_pts):
        key = i % 30 == 0
        frame = fake_video_frame(rng, key)
        c = _cluster_of(pts, cluster_ts)
        payload = block_payload(1, pts - cluster_ts[c], 0, [frame])
        refs = () if key else (-371, -742, -1113)
        groups[c].append(block_group(payload, key, 371, refs))
    for b in range(20):
        first = b * 78
        frames = [fake_audio_frame(rng) for _ in range(78)]
        pts = audio_pts[first]
        c = _cluster_of(pts, cluster_ts)
        payload = block_payload(2, pts - cluster_ts[c], 0x02, frames, lace="xiph")
        groups[c].append(
            block_group(payload, True, audio_ext[first + 78] - audio_pts[first])
        )
    clusters = [
        cluster(cluster_ts[i], groups[i], unknown=(i == 0)) for i in range(8)
    ]
    tracks = master(
        schema.ID_TRACKS,
        video_track_entry(fake_avcc()),
        audio_track_entry(fake_asc()),
    )
    return (
        ebml_header()
        + el(schema.ID_VOID, b"\x00" * 16)
        + master_unknown(
            schema.ID_SEGMENT, info(SCALE_US90, PTS_TOTAL_UNITS + 371), tracks,
            *clusters,
        )
    )


def build_bframes(rng):
    scale = 1_000_000
    pts_ms = [i * 33 for i in range(60)]
    order = []
    for g in range(0, 60, 4):
        order += [g, g + 3, g + 1, g + 2]
    cluster_ts = [c * 495 for c in range(4)]
    groups = [[] for _ in range(4)]
    for i in order:
        key = i % 4 == 0
        frame = fake_video_frame(rng, key)
        c = _cluster_of(pts_ms[i], cluster_ts)
        flags = 0x80 if key else 0x00
        payload = block_payload(1, pts_ms[i] - cluster_ts[c], flags, [frame])
        groups[c].append(el(schema.ID_SIMPLE_BLOCK, payload))
    audio_pts = [i * 21 for i in range(200)]
    for b in range(40):
        first = b * 5
        frames = [fake_audio_frame(rng) for _ in range(5)]
        pts = audio_pts[first]
        c = _cluster_of(pts, cluster_ts)
        payload = block_payload(2, pts - cluster_ts[c], 0x06, frames, lace="ebml")
        groups[c].append(block_group(payload, True, 105))
    cues = master(
        schema.ID_CUES,
        master(0xBB, uint_el(0xB3, 0),
               master(0xB7, uint_el(0xF7, 1), uint_el(0xF1, 0))),
    )
    clusters = [cluster(cluster_ts[i], groups[i]) for i in range(4)]
    tracks = master(
        schema.ID_TRACKS,
        video_track_entry(fake_avcc()),
        audio_track_entry(fake_asc()),
    )
    return (
        ebml_header()
        + master(schema.ID_SEGMENT, info(scale, 2000), tracks, *clusters, cues)
    )


def build_subtitles(rng):
    scale = 1_000_000
    cluster_ts = [0, 330, 660]
    groups = [[] for _ in range(3)]
    for i in range(30):
        pts = i * 33
        frame = fake_video_frame(rng, i % 15 == 0)
        c = _cluster_of(pts, cluster_ts)
        payload = block_payload(1, pts - cluster_ts[c], 0x80, [frame])
        groups[c].append(el(schema.ID_SIMPLE_BLOCK, payload))
    for b in range(10):
        frames = [b"\x21\x10" + bytes(198) for _ in range(10)]
        pts = b * 10 * 21
        c = _cluster_of(pts, cluster_ts)
        payload = block_payload(2, pts - cluster_ts[c], 0x04, frames, lace="fixed")
        groups[c].append(block_group(payload, True, 210))
    for i in range(10):
        pts = i * 1000
        c = _cluster_of(pts, cluster_ts)
        text = f"subtitle line {i}".encode("utf-8")
        payload = block_payload(3, pts - cluster_ts[c], 0x80, [text])
        groups[c].append(el(schema.ID_SIMPLE_BLOCK, payload))
    sub_entry = master(
        schema.ID_TRACK_ENTRY,
        uint_el(schema.ID_TRACK_NUMBER, 3),
        uint_el(0x73C5, 3),
        uint_el(schema.ID_TRACK_TYPE, 17),
        str_el(schema.ID_CODEC_ID, "S_TEXT/UTF8"),
        str_el(schema.ID_LANGUAGE, "und"),
    )
    tracks = master(
        schema.ID_TRACKS,
        video_track_entry(fake_avcc()),
        audio_track_entry(fake_asc()),
        sub_entry,
    )
    clusters = [cluster(cluster_ts[i], groups[i]) for i in range(3)]
    return ebml_header() + master(
        schema.ID_SEGMENT, info(scale, 10000), tracks, *clusters
    )


def build_bad_codec(rng):
    entry = master(
        schema.ID_TRACK_ENTRY,
        uint_el(schema.ID_TRACK_NUMBER, 1),
        uint_el(schema.ID_TRACK_TYPE, 1),
        str_el(schema.ID_CODEC_ID, "V_PRORES"),
    )
    payload = block_payload(1, 0, 0x80, [rng.randbytes(64)])
    cl = cluster(0, [el(schema.ID_SIMPLE_BLOCK, payload)])
    return ebml_header() + master(
        schema.ID_SEGMENT, info(1_000_000, 0),
        master(schema.ID_TRACKS, entry), cl,
    )


def build_missing_private(rng):
    entry = master(
        schema.ID_TRACK_ENTRY,
        uint_el(schema.ID_TRACK_NUMBER, 1),
        uint_el(schema.ID_TRACK_TYPE, 1),
        str_el(schema.ID_CODEC_ID, "V_MPEG4/ISO/AVC"),
    )
    payload = block_payload(1, 0, 0x80, [rng.randbytes(64)])
    cl = cluster(0, [el(schema.ID_SIMPLE_BLOCK, payload)])
    return ebml_header() + master(
        schema.ID_SEGMENT, info(1_000_000, 0),
        master(schema.ID_TRACKS, entry), cl,
    )


def build_truncated_block(rng):
    cluster_ts = [0, 500]
    groups = [[] for _ in range(2)]
    for i in range(30):
        pts = i * 33
        frame = fake_video_frame(rng, i % 30 == 0)
        c = _cluster_of(pts, cluster_ts)
        payload = block_payload(1, pts - cluster_ts[c], 0, [frame])
        groups[c].append(block_group(payload, i % 30 == 0, 33))
    frames = [fake_audio_frame(rng) for _ in range(78)]
    payload = block_payload(2, 0, 0x02, frames, lace="xiph")
    groups[0].append(block_group(payload, True, 1664))
    clusters = [cluster(cluster_ts[i], groups[i]) for i in range(2)]
    tracks = master(
        schema.ID_TRACKS,
        video_track_entry(fake_avcc()),
        audio_track_entry(fake_asc()),
    )
    data = ebml_header() + master_unknown(
        schema.ID_SEGMENT, info(1_000_000, 1000), tracks, *clusters
    )
    tail = block_group(block_payload(1, 990, 0, [fake_video_frame(rng, False)]), False, 33)
    return data + tail[: len(tail) // 2]


def build_jitter(rng):
    pts = [i * 21 for i in range(200)]
    pts[51] = pts[50]
    pts[150] = pts[149] - 500
    pts[161] = pts[160]
    pts[171] = pts[170]
    cluster_ts = [0, 2100]
    groups = [[] for _ in range(2)]
    for i, p in enumerate(pts):
        frame = fake_audio_frame(rng)
        c = _cluster_of(p, cluster_ts)
        payload = block_payload(2, p - cluster_ts[c], 0x80, [frame])
        groups[c].append(el(schema.ID_SIMPLE_BLOCK, payload))
    clusters = [cluster(cluster_ts[i], groups[i]) for i in range(2)]
    tracks = master(schema.ID_TRACKS, audio_track_entry(fake_asc()))
    return ebml_header() + master(
        schema.ID_SEGMENT, info(1_000_000, 4200), tracks, *clusters
    )


def build_bad_timestamps(rng):
    groups = []
    for _ in range(50):
        payload = block_payload(2, 0, 0x80, [fake_audio_frame(rng)])
        groups.append(el(schema.ID_SIMPLE_BLOCK, payload))
    tracks = master(schema.ID_TRACKS, audio_track_entry(fake_asc()))
    return ebml_header() + master(
        schema.ID_SEGMENT, info(1_000_000, 0), tracks, cluster(0, groups)
    )


def build_scale_10ms(rng):
    """Ten-millisecond ticks plus lacing without BlockDuration."""
    scale = 10_000_000
    children = []
    for tick in range(30):
        key = tick % 10 == 0
        payload = block_payload(
            1, tick, 0x80 if key else 0, [fake_video_frame(rng, key)]
        )
        children.append(el(schema.ID_SIMPLE_BLOCK, payload))
    audio_frames = 0
    for block in range(26):
        # 5 * 1024 / 48000 s is 10.666... ticks at a 10 ms timescale.
        block_ts = (block * 32) // 3
        frames = [fake_audio_frame(rng) for _ in range(5)]
        payload = block_payload(2, block_ts, 0x82, frames, lace="xiph")
        children.append(el(schema.ID_SIMPLE_BLOCK, payload))
        audio_frames += len(frames)
    tracks = master(
        schema.ID_TRACKS,
        video_track_entry(fake_avcc()),
        audio_track_entry(fake_asc(), rate=48000),
    )
    return ebml_header() + master(
        schema.ID_SEGMENT,
        info(scale, 320),
        tracks,
        cluster(0, children),
    )


def build_audio_offset(rng):
    children = []
    for i in range(30):
        key = i % 15 == 0
        payload = block_payload(1, i * 33, 0x80 if key else 0,
                                [fake_video_frame(rng, key)])
        children.append(el(schema.ID_SIMPLE_BLOCK, payload))
    for i in range(20):
        payload = block_payload(2, 100 + i * 21, 0x80,
                                [fake_audio_frame(rng)])
        children.append(el(schema.ID_SIMPLE_BLOCK, payload))
    tracks = master(
        schema.ID_TRACKS,
        video_track_entry(fake_avcc()),
        audio_track_entry(fake_asc()),
    )
    return ebml_header() + master(
        schema.ID_SEGMENT,
        info(1_000_000, 1000),
        tracks,
        cluster(0, children),
    )


def build_all(out_dir, seed=7):
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    rng = random.Random(seed)
    files = {
        "unknown_len.mkv": build_unknown_len(rng),
        "bframes.mkv": build_bframes(rng),
        "subtitles.mkv": build_subtitles(rng),
        "bad_codec.mkv": build_bad_codec(rng),
        "missing_private.mkv": build_missing_private(rng),
        "not_mkv.bin": b"This is not a Matroska file.\n",
        "truncated_header.mkv": ebml_header()[:10],
        "truncated_block.mkv": build_truncated_block(rng),
        "jitter.mkv": build_jitter(rng),
        "bad_timestamps.mkv": build_bad_timestamps(rng),
        "scale_10ms.mkv": build_scale_10ms(rng),
        "audio_offset.mkv": build_audio_offset(rng),
    }
    for name, data in files.items():
        (out / name).write_bytes(data)
    return sorted(files)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True)
    parser.add_argument("--seed", type=int, default=7)
    args = parser.parse_args(argv)
    names = build_all(args.out, args.seed)
    for name in names:
        print(f"wrote {Path(args.out) / name}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
