#!/usr/bin/env python3
"""Generate deterministic, self-contained Matroska fixtures."""

from __future__ import annotations

import argparse
import os
import sys
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from fixture_builder import (  # noqa: E402
    block_group, chapter_atom, chapters, cluster, cues, ebml_header, element,
    info, simple_block, tags, track_entry, tracks, uint_element, void,
)
from mkvmp4.ebml.ids import SEGMENT  # noqa: E402
from mkvmp4.ebml.reader import EBMLReader  # noqa: E402

VIDEO_DURATION = 33_333_333
SECOND_DURATION = 33_311_036
AUDIO_DURATION = 23_218_605
CLUSTER_MS = (0, 2000, 4000, 6000, 8000)


@dataclass(slots=True)
class FixtureSample:
    track: int
    index: int
    pts_ns: int
    order_ns: int
    size: int
    keyframe: bool = True


def _payload(track: int, index: int, size: int) -> bytes:
    if track == 4:
        return f"subtitle {index + 1}".encode("utf-8").ljust(size, b" ")[:size]
    seed = bytes(((track * 37 + index + offset) & 0xFF for offset in range(32)))
    repeats = (size + 31) // 32
    return (seed * repeats)[:size]


def _video_times(count: int, *, restart: bool) -> list[int]:
    nominal = VIDEO_DURATION if restart else SECOND_DURATION
    changes = {
        40: -nominal, 100: -2 * nominal, 150: 2 * nominal,
        200: -nominal, 250: 2 * nominal, 280: 2 * nominal,
    } if restart else {}
    result: list[int] = []
    shift = 0
    for index in range(count):
        shift += changes.get(index, 0)
        result.append(index * nominal + shift)
    return result


def _audio_times(count: int) -> list[int]:
    return [round(index * 9_984_000_000 / (count - 1)) for index in range(count)]


def _subtitle_times(count: int) -> list[int]:
    return [index * 500_000_000 for index in range(count)]


def _build_track_blocks(samples: list[FixtureSample], *, default_ns: int,
                        lace: int, single_indices: set[int],
                        with_group: int | None = None) -> list[tuple[int, int, bytes]]:
    blocks: list[tuple[int, int, bytes]] = []
    cursor = 0
    while cursor < len(samples):
        sample = samples[cursor]
        cluster_index = min(4, max(0, sample.pts_ns // 2_000_000_000))
        if cursor in single_indices:
            count = 1
        else:
            max_count = 5 if lace == 1 else (5 if lace == 2 else 6)
            count = 1
            while (count < max_count and cursor + count < len(samples)
                   and cursor + count not in single_indices
                   and min(4, max(0, samples[cursor + count].pts_ns // 2_000_000_000)) == cluster_index):
                count += 1
        group = samples[cursor:cursor + count]
        frames = [_payload(item.track, item.index, item.size) for item in group]
        first = group[0]
        relative = round(first.pts_ns / 1_000_000) - CLUSTER_MS[cluster_index]
        if with_group is not None and cursor == with_group:
            block = block_group(first.track, relative, frames[0], 1, [1])
        elif count == 1:
            block = simple_block(first.track, relative, frames, 0, first.keyframe)
        else:
            block = simple_block(first.track, relative, frames, lace, first.keyframe)
        blocks.append((first.order_ns, first.pts_ns, block))
        cursor += count
    return blocks


def _build_full(path: Path, *, target_elements: int = 613,
                unknown_segment: bool = True, unknown_clusters: bool = True,
                crc: bool = False, repair: bool = True) -> None:
    video_times = _video_times(300, restart=repair)
    second_times = _video_times(300, restart=False)
    if repair:
        video_times[-1] = 10_026_000_000
    audio_times = _audio_times(431)
    subtitle_times = _subtitle_times(18)
    track_samples = {
        1: [FixtureSample(1, i, ts, i * VIDEO_DURATION, 42_344 if i == 0 else 40_000)
            for i, ts in enumerate(video_times)],
        2: [FixtureSample(2, i, ts, i * SECOND_DURATION, 39_500)
            for i, ts in enumerate(second_times)],
        3: [FixtureSample(3, i, ts, ts, 340) for i, ts in enumerate(audio_times)],
        4: [FixtureSample(4, i, ts, ts, 60) for i, ts in enumerate(subtitle_times)],
    }
    video_single = ({39, 40, 99, 100, 149, 150, 199, 200, 249, 250, 279, 280, 299}
                    if repair else {299})
    block_sets = {
        1: _build_track_blocks(track_samples[1], default_ns=VIDEO_DURATION,
                               lace=1, single_indices=video_single),
        2: _build_track_blocks(track_samples[2], default_ns=VIDEO_DURATION,
                               lace=2, single_indices={299}),
        3: _build_track_blocks(track_samples[3], default_ns=AUDIO_DURATION,
                               lace=3, single_indices={426, 427, 428, 429, 430}),
        4: _build_track_blocks(track_samples[4], default_ns=500_000_000,
                               lace=0, single_indices=set(range(18))),
    }
    cluster_payloads: list[bytes] = []
    for cluster_index, timestamp_ms in enumerate(CLUSTER_MS):
        candidates: list[tuple[int, int, bytes]] = []
        for number in range(1, 5):
            candidates.extend((order, pts, data) for order, pts, data in block_sets[number]
                              if min(4, max(0, pts // 2_000_000_000)) == cluster_index)
        candidates.sort(key=lambda item: item[0])
        cluster_payloads.append(cluster(timestamp_ms, [data for _, _, data in candidates],
                                        unknown=unknown_clusters, with_crc=crc))
    private_avc = bytes.fromhex("0164001FFFE1001F6764001FE588800A03D9C183000300010001000168EBE3CB22C0")
    track_blobs = [
        track_entry(1, 1, "V_MPEG4/ISO/AVC", private_avc, duration_ns=VIDEO_DURATION,
                    width=640, height=360),
        track_entry(2, 1, "V_MPEG4/ISO/AVC", private_avc, name="屏幕共享",
                    duration_ns=VIDEO_DURATION, width=1280, height=720),
        track_entry(3, 2, "A_AAC", bytes.fromhex("1210"), language="eng",
                    duration_ns=AUDIO_DURATION, sample_rate=48000, channels=2),
        track_entry(4, 17, "S_TEXT/UTF8", b"", language="zho", forced=True,
                    duration_ns=500_000_000),
    ]
    atoms = [chapter_atom(index + 1, index * 2000, min(9960, (index + 1) * 2000),
                          f"Chapter {index + 1}") for index in range(6)]
    body = b"".join([
        info(1_000_000, 9960.0, "Restart recovery sample"),
        tracks(track_blobs), *cluster_payloads, cues(5), chapters(atoms),
        tags([("ENCODER", "mkvmp4 fixture"), ("SCENE", "restart")]),
    ])
    data = ebml_header() + element(SEGMENT, body, unknown=unknown_segment)
    path.write_bytes(data)
    if target_elements and unknown_segment:
        with EBMLReader(path) as reader:
            count = reader.walk().elements
        if count < target_elements:
            with open(path, "ab") as stream:
                stream.write(void() * (target_elements - count))
        elif count > target_elements:
            raise RuntimeError(f"fixture base has {count} elements, above target {target_elements}")


def _build_small(path: Path, *, codec: str = "V_MPEG4/ISO/AVC",
                 crc: bool = False, group: bool = False) -> None:
    private = bytes.fromhex("0164001FFFE1001F6764001FE588800A03D9C183000300010001000168EBE3CB22C0") \
        if codec.startswith("V_") else bytes.fromhex("1210")
    tracks_blob = tracks([track_entry(1, 1 if codec.startswith("V_") else 2, codec,
                                      private, duration_ns=40_000_000, width=320,
                                      height=180, sample_rate=48000, channels=2)])
    blocks = []
    for index in range(10):
        if group and index == 5:
            blocks.append(block_group(1, index * 200, _payload(1, index, 1000),
                                      1, [1], keyframe=False))
        else:
            frames = [_payload(1, index * 5 + lace, 500) for lace in range(5)]
            blocks.append(simple_block(1, index * 200, frames,
                                       1 if index % 3 == 0 else 2 if index % 3 == 1 else 3))
    cluster_blob = cluster(0, blocks, unknown=False, with_crc=crc)
    body = info(1_000_000, 400.0, "Basic sample") + tracks_blob + cluster_blob
    path.write_bytes(ebml_header() + element(SEGMENT, body, unknown=False))


def generate(out_dir: Path, cases: int) -> list[Path]:
    out_dir.mkdir(parents=True, exist_ok=True)
    outputs = [out_dir / "case_a_basic.mkv", out_dir / "case_b_restart.mkv",
               out_dir / "case_c_lace.mkv", out_dir / "case_d_unsupported.mkv"]
    if cases >= 1:
        _build_small(outputs[0])
    if cases >= 2:
        _build_full(outputs[1])
    if cases >= 3:
        _build_small(outputs[2], crc=True)
    if cases >= 4:
        _build_small(outputs[3], codec="A_OPUS")
    return outputs[:max(0, min(cases, 4))]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", default="samples")
    parser.add_argument("--cases", type=int, default=4)
    args = parser.parse_args()
    for path in generate(Path(args.out), args.cases):
        print(f"wrote {path} ({os.path.getsize(path)} bytes)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

