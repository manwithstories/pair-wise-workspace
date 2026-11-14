"""Generate deterministic Matroska fixtures without external binaries."""

from __future__ import annotations

import argparse
import hashlib
import json
import random
from pathlib import Path

from ebml_builder import (
    audio_track,
    crc32_master,
    ebml_header,
    element,
    info,
    master,
    simple_block,
    subtitle_track,
    tags_for_depth,
    uint,
    video_track,
)

AVC_PRIVATE = bytes.fromhex("0142001effe100046742001e01000468ce3c80")
AAC_PRIVATE = bytes.fromhex("1210")
VIDEO_CODEC = "V_MPEG4/ISO/AVC"


def _data(rng: random.Random, size: int) -> bytes:
    return bytes(rng.getrandbits(8) for _ in range(size))


def _video_frames(rng: random.Random, count: int) -> list[tuple[float, bytes, bool]]:
    times: list[float] = []
    cycle = (-40.0, 80.0, 0.0, 40.0)
    for index in range(count):
        timestamp = (index // 4) * 160.0 + cycle[index % 4]
        if index and index % 37 == 0:
            timestamp = times[-1]
        times.append(timestamp)
    return [(timestamp, _data(rng, 48 + rng.getrandbits(5)), index % 25 == 0) for index, timestamp in enumerate(times)]


def _audio_frames(rng: random.Random, count: int) -> list[tuple[float, bytes, bool]]:
    return [(-40.0 + index * 40.0, _data(rng, 16 + rng.getrandbits(4)), True) for index in range(count)]


def _small_segment(
    rng: random.Random,
    *,
    frames: int = 60,
    cues: str | None = None,
    crc_corrupt: bool = False,
    include_tracks: bool = True,
    include_cluster: bool = True,
    unknown_cluster: bool = False,
) -> bytes:
    tracks = []
    if include_tracks:
        tracks = [
            video_track(1, 101, 640, 360, default_duration_ns=40_000_000, codec_private=AVC_PRIVATE),
            audio_track(2, 102, default_duration_ns=40_000_000, codec_private=AAC_PRIVATE),
            subtitle_track(3, 103),
        ]
    prefix = [info(frames * 40.0)]
    if tracks:
        prefix.append(master(0x1654AE6B, tracks))
    cluster_bytes: list[bytes] = []
    cluster_offsets: list[int] = []
    cursor = sum(len(item) for item in prefix) + (6 if crc_corrupt else 0)
    if include_cluster:
        per_cluster = 10
        for cluster_index, start in enumerate(range(0, frames, per_cluster)):
            cluster_time = cluster_index * per_cluster * 40
            children = [element(0xE7, uint(cluster_time))]
            for index in range(start, min(start + per_cluster, frames)):
                relative = index * 40 - cluster_time - (40 if index == 0 else 0)
                key = index % per_cluster == 0
                children.append(
                    simple_block(1, relative, _data(rng, 40 + rng.getrandbits(4)), keyframe=key)
                )
                children.append(
                    simple_block(2, relative, _data(rng, 16 + rng.getrandbits(3)), keyframe=True)
                )
            cluster_offsets.append(cursor)
            cluster = master(0x1F43B675, children, unknown=unknown_cluster)
            cluster_bytes.append(cluster)
            cursor += len(cluster)
    body_children = list(prefix) + cluster_bytes
    if cues:
        cue_points = []
        for index, offset in enumerate(cluster_offsets):
            position = offset if cues == "valid" or index % 2 == 0 else len(cluster_bytes) + offset + 10_000
            cue_points.append(
                master(
                    0xBB,
                    [
                        element(0xB3, uint(index * 400)),
                        master(
                            0xB7,
                            [
                                element(0xF7, uint(1)),
                                element(0xF1, uint(position)),
                                element(0xF0, uint(0)),
                            ],
                        ),
                    ],
                )
            )
        body_children.append(master(0x1C53BB6B, cue_points))
    if crc_corrupt:
        return crc32_master(0x18538067, body_children, corrupt=True)
    return master(0x18538067, body_children)


def _live_unknown(rng: random.Random) -> bytes:
    video = _video_frames(rng, 5600)
    audio = _audio_frames(rng, 5600)
    tracks = [
        video_track(1, 201, 1280, 720, default_duration_ns=40_000_000, codec_private=AVC_PRIVATE),
        audio_track(2, 202, default_duration_ns=40_000_000, codec_private=AAC_PRIVATE),
        subtitle_track(3, 203),
    ]
    try:
        info_bytes = info(5600 * 40.0)
        tracks_bytes = master(0x1654AE6B, tracks)
    except Exception as exc:
        raise RuntimeError("live fixture metadata build failed") from exc
    clusters: list[bytes] = []
    cluster_blocks = 32
    for cluster_index in range(0, 5600, 16):
        cluster_time = cluster_index * 40
        children = [element(0xE7, uint(cluster_time))]
        for local in range(16):
            video_timestamp, video_data, video_key = video[cluster_index + local]
            audio_timestamp, audio_data, _ = audio[cluster_index + local]
            children.append(
                simple_block(1, int(round(video_timestamp)) - cluster_time, video_data, keyframe=video_key)
            )
            children.append(
                simple_block(2, int(round(audio_timestamp)) - cluster_time, audio_data, keyframe=True)
            )
        clusters.append(master(0x1F43B675, children, unknown=True))
    body = info_bytes + tracks_bytes + tags_for_depth() + b"".join(clusters)
    return ebml_header() + element(0x18538067, body, unknown=True)


def _multi_segment(rng: random.Random) -> bytes:
    first = _small_segment(rng, frames=20)
    second = _small_segment(rng, frames=20)
    return ebml_header() + first + second


def generate(out_dir: Path, seed: int) -> dict[str, object]:
    out_dir.mkdir(parents=True, exist_ok=True)
    rng = random.Random(seed)
    files: dict[str, bytes] = {}
    files["live_unknown.mkv"] = _live_unknown(rng)
    files["no_cues.mkv"] = ebml_header() + _small_segment(rng, frames=60)
    files["negative_dts.mkv"] = ebml_header() + _small_segment(rng, frames=50)
    files["with_cues.mkv"] = ebml_header() + _small_segment(rng, frames=60, cues="valid")
    files["bad_cues.mkv"] = ebml_header() + _small_segment(rng, frames=60, cues="bad")
    files["multi_segment.mkv"] = _multi_segment(rng)
    files["crc_bad.mkv"] = ebml_header() + _small_segment(rng, frames=20, crc_corrupt=True)
    files["no_tracks.mkv"] = ebml_header() + _small_segment(rng, frames=20, include_tracks=False)
    files["zero_samples.mkv"] = ebml_header() + _small_segment(rng, frames=0, include_cluster=False)
    valid = ebml_header() + _small_segment(rng, frames=20)
    files["truncated.mkv"] = valid[:-23]
    manifest: dict[str, object] = {"seed": seed, "files": []}
    for name, data in files.items():
        (out_dir / name).write_bytes(data)
        manifest["files"].append(
            {
                "name": name,
                "size": len(data),
                "sha256": hashlib.sha256(data).hexdigest(),
            }
        )
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return manifest


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default="fixtures")
    parser.add_argument("--seed", type=int, default=7)
    args = parser.parse_args()
    manifest = generate(Path(args.out), args.seed)
    print(json.dumps({"ok": True, "out": args.out, "seed": args.seed, "files": manifest["files"]}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
