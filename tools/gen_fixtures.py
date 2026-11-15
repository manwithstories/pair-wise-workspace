#!/usr/bin/env python3
"""Synthetic Matroska fixture generator (no external assets).

Generates deterministic MKV files exercising the remuxer edge cases:
  * unknown_size  - Segment + two Clusters written with unknown length
  * cues_missing  - no Cues element at all
  * b_frame       - video stored in decode order (I B P reorder); this
                    file uses 1 ms timecode units because the reorder
                    span exceeds the int16 block-timecode range of
                    finer scales (mirrors real-world MKV)
  * lacing        - audio frames Xiph-laced 8 per block

Every file holds 1800 video frames (30 fps, 60 s) and 2813 AAC frames
(1024 samples @ 48 kHz), and is padded with nested SimpleTag elements
to exactly TARGET_ELEMENTS elements so probe statistics are stable.
"""

import argparse
import random
import struct
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from mkv2mp4.ebml.vint import encode_id, encode_size  # noqa: E402

VIDEO_FRAMES = 1800          # 30 fps * 60 s
AUDIO_FRAMES = 2813          # 2813 * 1024 / 48000 ~ 60.01 s
KEYFRAME_INTERVAL = 90
LACE_GROUP = 8
TARGET_ELEMENTS = 120000
ALL_CASES = ("unknown_size", "cues_missing", "b_frame", "lacing")

# case -> (TimecodeScale in ns, cluster span in timecode units)
_CASE_CONFIG = {
    "unknown_size": (1000, 30000),      # 1 us units, 30 ms clusters
    "cues_missing": (1000, 30000),
    "lacing": (1000, 30000),
    "b_frame": (1_000_000, 1000),       # 1 ms units, 1 s clusters
}


class _Counter:
    def __init__(self):
        self.n = 0


def _round_div(a, b):
    return (2 * a + b) // (2 * b)


def _video_pts(i, scale):
    """Presentation time of video frame i in timecode units."""
    return _round_div(i * 1_000_000_000, 30 * scale)


def _audio_pts(j, scale):
    """Presentation time of AAC frame j in timecode units."""
    return _round_div(j * 1024 * 1_000_000_000, 48000 * scale)


def _decode_order(n):
    """Presentation indices in decode order: I B P B P ... (delay 1)."""
    order = [0]
    i = 1
    while i + 1 < n:
        order += [i + 1, i]
        i += 2
    if i < n:
        order.append(i)
    return order


def _uint(eid, value, counter, length=None):
    counter.n += 1
    if length is None:
        length = max(1, (value.bit_length() + 7) // 8)
    return encode_id(eid) + encode_size(length) + value.to_bytes(length, "big")


def _float(eid, value, counter):
    counter.n += 1
    return encode_id(eid) + encode_size(8) + struct.pack(">d", value)


def _text(eid, value, counter):
    data = value.encode("utf-8")
    counter.n += 1
    return encode_id(eid) + encode_size(len(data)) + data


def _binary(eid, data, counter):
    counter.n += 1
    return encode_id(eid) + encode_size(len(data)) + data


def _master(eid, payload, counter, unknown_len=0):
    counter.n += 1
    if unknown_len:
        return encode_id(eid) + encode_size(None, unknown_len) + payload
    return encode_id(eid) + encode_size(len(payload)) + payload


def _ebml_header(counter):
    payload = _uint(0x4286, 1, counter) + _uint(0x42F7, 1, counter)
    payload += _uint(0x42F2, 4, counter) + _uint(0x42F3, 8, counter)
    payload += _text(0x4282, "matroska", counter)
    payload += _uint(0x4287, 4, counter) + _uint(0x4285, 2, counter)
    return _master(0x1A45DFA3, payload, counter)


def _info(counter, scale):
    duration = 60010.667 if scale >= 1_000_000 else 60010667.0
    payload = _uint(0x2AD7B1, scale, counter)
    payload += _float(0x4489, duration, counter)
    payload += _text(0x4D80, "mkv2mp4-fixture", counter)
    payload += _text(0x5741, "gen_fixtures.py", counter)
    payload += _text(0x7BA9, "synthetic", counter)
    return _master(0x1549A966, payload, counter)


def _fake_avcc(rng):
    sps = rng.randbytes(16)
    pps = rng.randbytes(4)
    return (b"\x01\x42\x00\x1e\xff\xe1" + struct.pack(">H", len(sps)) + sps
            + b"\x01" + struct.pack(">H", len(pps)) + pps)


def _tracks(counter, rng):
    video = _uint(0xD7, 1, counter) + _uint(0x73C5, 0x1111, counter)
    video += _uint(0x83, 1, counter) + _uint(0xB9, 1, counter)
    video += _uint(0x88, 1, counter) + _uint(0x9C, 1, counter)
    video += _text(0x536E, "main-video", counter)
    video += _text(0x22B59C, "eng", counter)
    video += _text(0x86, "V_MPEG4/ISO/AVC", counter)
    video += _binary(0x63A2, _fake_avcc(rng), counter)
    video += _uint(0x23E383, 33333333, counter)
    vs = _uint(0xB0, 1920, counter) + _uint(0xBA, 1080, counter)
    vs += _uint(0x54B0, 1920, counter) + _uint(0x54BA, 1080, counter)
    video += _master(0xE0, vs, counter)
    audio = _uint(0xD7, 2, counter) + _uint(0x73C5, 0x2222, counter)
    audio += _uint(0x83, 2, counter) + _uint(0x88, 1, counter)
    audio += _text(0x536E, "main-audio", counter)
    audio += _text(0x22B59C, "eng", counter)
    audio += _text(0x86, "A_AAC", counter)
    audio += _binary(0x63A2, b"\x11\x90", counter)  # AAC-LC 48kHz stereo
    audio += _uint(0x23E383, 21333333, counter)
    au = _float(0xB5, 48000.0, counter) + _uint(0x9F, 2, counter)
    au += _uint(0x6264, 16, counter)
    audio += _master(0xE1, au, counter)
    payload = _master(0xAE, video, counter) + _master(0xAE, audio, counter)
    return _master(0x1654AE6B, payload, counter)


def _video_payload(rng, keyframe):
    size = (6000 + rng.randrange(800)) if keyframe \
        else 1400 + rng.randrange(400)
    nal_type = 5 if keyframe else 1
    return struct.pack(">I", size - 4) + bytes([0x60 | nal_type]) \
        + rng.randbytes(size - 5)


def _audio_payload(rng, index):
    return rng.randbytes(96 + index % 5)


def _simple_block(track_number, flags, payloads, lace):
    """Block payload; the 2-byte relative timecode is patched in later."""
    data = encode_size(track_number) + b"\0\0" + bytes([flags])
    if lace and len(payloads) > 1:
        data += bytes([len(payloads) - 1])
        for payload in payloads[:-1]:
            size = len(payload)
            while size >= 255:
                data += b"\xff"
                size -= 255
            data += bytes([size])
    return data + b"".join(payloads)


def _frames(case, scale, rng):
    """Return [(decode_tc, pts_tc, track, block, keyframe)] in file order."""
    items = []
    order = _decode_order(VIDEO_FRAMES) if case == "b_frame" \
        else list(range(VIDEO_FRAMES))
    for decode_idx, display_idx in enumerate(order):
        keyframe = display_idx % KEYFRAME_INTERVAL == 0
        flags = 0x80 if keyframe else 0x00
        block = _simple_block(1, flags, [_video_payload(rng, keyframe)], False)
        items.append([_video_pts(decode_idx, scale),
                      _video_pts(display_idx, scale), 1, block, keyframe])
    if case == "lacing":
        group = []
        for j in range(AUDIO_FRAMES):
            group.append(_audio_payload(rng, j))
            if len(group) == LACE_GROUP or j == AUDIO_FRAMES - 1:
                pts = _audio_pts(j + 1 - len(group), scale)
                block = _simple_block(2, 0x80 | 0x02, group, True)
                items.append([pts, pts, 2, block, True])
                group = []
    else:
        for j in range(AUDIO_FRAMES):
            pts = _audio_pts(j, scale)
            block = _simple_block(2, 0x80, [_audio_payload(rng, j)], False)
            items.append([pts, pts, 2, block, True])
    items.sort(key=lambda f: (f[0], f[2]))
    return items


def _clusters(case, counter, items, cluster_span):
    """Pack blocks into clusters keyed by decode time; patch timecodes."""
    clusters = {}
    keyframe_clusters = []
    for decode_tc, pts_tc, track, block, keyframe in items:
        index = decode_tc // cluster_span
        rel = pts_tc - index * cluster_span
        if not -32768 <= rel <= 32767:
            raise ValueError("relative timecode %d out of int16 range" % rel)
        track_len = len(encode_size(track))
        block = block[:track_len] \
            + rel.to_bytes(2, "big", signed=True) + block[track_len + 2:]
        element = encode_id(0xA3) + encode_size(len(block)) + block
        clusters.setdefault(index, []).append(element)
        if track == 1 and keyframe:
            keyframe_clusters.append((pts_tc, index))
    unknown = {5, 17} if case == "unknown_size" else set()
    blobs = []
    for index in sorted(clusters):
        payload = _uint(0xE7, index * cluster_span, counter)
        payload += b"".join(clusters[index])
        blob = _master(0x1F43B675, payload, counter,
                       unknown_len=1 if index in unknown else 0)
        blobs.append((index, blob))
    counter.n += len(items)  # each SimpleBlock is one element
    return blobs, keyframe_clusters


def _cues(counter, keyframe_clusters, cluster_offsets):
    points = b""
    for pts_tc, cluster_index in keyframe_clusters:
        pos = _uint(0xF7, 1, counter)
        pos += _uint(0xF1, cluster_offsets[cluster_index], counter, length=4)
        pos += _uint(0x5378, 1, counter)
        point = _uint(0xB3, pts_tc, counter, length=4)
        point += _master(0xB7, pos, counter)
        points += _master(0xBB, point, counter)
    return _master(0x1C53BB6B, points, counter)


def _padding_tags(counter, needed):
    """Emit exactly ``needed`` elements as nested SimpleTag padding."""
    units, rem = divmod(needed - 3, 4)
    if units < 1:
        raise ValueError("padding budget too small: %d" % needed)
    parts = []
    for i in range(units):
        name = _text(0x45A3, "PAD", counter)
        nested = _master(0x67C8, _text(0x45A3, "x", counter), counter)
        extra = b""
        if i == 0:
            for _ in range(rem):
                extra += _text(0x4487, "p", counter)
        parts.append(_master(0x67C8, name + nested + extra, counter))
    body = _master(0x63C0, b"", counter)          # Targets
    tag = _master(0x7373, body + b"".join(parts), counter)
    return _master(0x1254C367, tag, counter)


def build_case(case, seed):
    scale, cluster_span = _CASE_CONFIG[case]
    rng = random.Random(seed)
    counter = _Counter()
    header = _ebml_header(counter)
    info = _info(counter, scale)
    tracks = _tracks(counter, rng)
    items = _frames(case, scale, rng)
    blobs, keyframe_clusters = _clusters(case, counter, items, cluster_span)
    cues = b"" if case == "cues_missing" else _cues(
        counter, keyframe_clusters, {i: 0 for i, _ in blobs})
    padding = _padding_tags(counter, TARGET_ELEMENTS - counter.n - 1)
    prefix = info + tracks + padding + cues
    offsets = {}
    pos = len(prefix)
    for index, blob in blobs:
        offsets[index] = pos
        pos += len(blob)
    if case != "cues_missing":
        cues = _cues(_Counter(), keyframe_clusters, offsets)
        prefix = info + tracks + padding + cues
    payload = prefix + b"".join(blob for _, blob in blobs)
    unknown_len = 8 if case == "unknown_size" else 0
    segment = _master(0x18538067, payload, counter, unknown_len=unknown_len)
    return header + segment


def generate(out_dir, seed=7, cases=ALL_CASES):
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    written = []
    for case in cases:
        if case not in ALL_CASES:
            raise ValueError("unknown case %r (choose from %s)"
                             % (case, ", ".join(ALL_CASES)))
        data = build_case(case, seed)
        path = out_dir / ("%s.mkv" % case)
        path.write_bytes(data)
        written.append(path)
        print("wrote %s (%d bytes)" % (path, len(data)))
    return written


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True)
    parser.add_argument("--seed", type=int, default=7)
    parser.add_argument("--cases", default=",".join(ALL_CASES))
    args = parser.parse_args(argv)
    generate(args.out, args.seed, args.cases.split(","))
    return 0


if __name__ == "__main__":
    sys.exit(main())
