#!/usr/bin/env python3
"""Generate deterministic P6 clips, a timeline, and a structural expectation."""

from __future__ import annotations

import argparse
import json
import shutil
from fractions import Fraction
from pathlib import Path

KERNELS = [
    "linear_blend",
    "threshold_mask",
    "channel_key",
    "geometry_resample",
    "block_permute",
]
RATES = [(1, 2), (2, 1), (3, 1), (0, 1), (-1, 1), (1, 1)]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Generate P6 transition fixture")
    parser.add_argument("--out", required=True, help="fixture output directory")
    parser.add_argument("--width", type=int, default=64)
    parser.add_argument("--height", type=int, default=48)
    parser.add_argument("--clips", type=int, default=4)
    parser.add_argument("--frames", type=int, default=36)
    parser.add_argument("--overlap", type=int, default=4)
    parser.add_argument("--seed", type=int, default=7)
    args = parser.parse_args()
    if not 0 < args.width <= 4096 or not 0 < args.height <= 4096:
        parser.error("width and height must be in 1..4096")
    if args.clips < 2 or args.clips > 16:
        parser.error("clips must be in 2..16")
    if args.frames < 12 or args.frames > 10000:
        parser.error("frames must be in 12..10000")
    if not 1 <= args.overlap < args.frames:
        parser.error("overlap must be in 1..frames-1")
    if not 0 <= args.seed <= 0xFFFFFFFFFFFFFFFF:
        parser.error("seed must fit uint64")
    return args


def speed_schedule(frames: int) -> list[dict[str, object]]:
    base = frames // 6
    if base < 1:
        raise ValueError("frames are too short for six speed intervals")
    durations = [base] * 5 + [frames - base * 5]
    source_position = Fraction(0, 1)
    result: list[dict[str, object]] = []
    for (num, den), duration in zip(RATES, durations):
        source_start = source_position.numerator // source_position.denominator
        result.append(
            {
                "frames": duration,
                "source": source_start,
                "rate": [num, den],
            }
        )
        source_position += Fraction(duration * num, den)
    if source_position < 0 or source_position > frames:
        raise ValueError("generated speed schedule leaves the source clip")
    return result


def splitmix64(value: int) -> int:
    value = (value + 0x9E3779B97F4A7C15) & 0xFFFFFFFFFFFFFFFF
    value = ((value ^ (value >> 30)) * 0xBF58476D1CE4E5B9) & 0xFFFFFFFFFFFFFFFF
    value = ((value ^ (value >> 27)) * 0x94D049BB133111EB) & 0xFFFFFFFFFFFFFFFF
    return value ^ (value >> 31)


def frame_bytes(width: int, height: int, clip: int, frame: int, seed: int) -> bytes:
    pixels = bytearray(width * height * 3)
    offset = 0
    for y in range(height):
        for x in range(width):
            checker = ((x // 4 + y // 4 + frame + clip) & 1) * 37
            noise = splitmix64(seed ^ (clip << 48) ^ (frame << 24) ^ (y << 12) ^ x)
            red = (x * 5 + y * 3 + frame * 7 + clip * 31 + checker) & 0xFF
            green = (x * 11 + y * 7 + frame * 5 + clip * 47 + (noise & 0x1F)) & 0xFF
            blue = ((x ^ (y * 3)) * 13 + frame * 9 + clip * 61 + checker) & 0xFF
            pixels[offset] = red
            pixels[offset + 1] = green
            pixels[offset + 2] = blue
            offset += 3
    return bytes(pixels)


def write_ppm(path: Path, width: int, height: int, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(f"P6\n{width} {height}\n255\n".encode("ascii") + data)


def main() -> int:
    args = parse_args()
    output = Path(args.out)
    output.mkdir(parents=True, exist_ok=True)
    frames_root = output / "frames"
    if frames_root.exists():
        shutil.rmtree(frames_root)
    frames_root.mkdir()
    schedule = speed_schedule(args.frames)

    clips = []
    for clip_index in range(args.clips):
        clip_id = f"c{clip_index}"
        clip_dir = frames_root / clip_id
        clip_dir.mkdir()
        for frame_index in range(args.frames):
            data = frame_bytes(args.width, args.height, clip_index, frame_index, args.seed)
            write_ppm(clip_dir / f"{frame_index:06d}.ppm", args.width, args.height, data)
        clips.append(
            {
                "id": clip_id,
                "dir": f"c{clip_index}",
                "frames": args.frames,
                "start": clip_index * (args.frames - args.overlap),
            }
        )

    transitions = []
    for index in range(args.clips - 1):
        transitions.append(
            {
                "from": index,
                "to": index + 1,
                "class": KERNELS[index % len(KERNELS)],
                "start": clips[index + 1]["start"],
                "frames": args.overlap,
                "priority": index + 1,
            }
        )
    total_frames = args.frames + (args.clips - 1) * (args.frames - args.overlap)
    timeline = {
        "version": 1,
        "width": args.width,
        "height": args.height,
        "seed": args.seed,
        "speed": schedule,
        "clips": clips,
        "transitions": transitions,
    }
    (output / "tl.txt").write_text(
        json.dumps(timeline, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    expectation = {
        "width": args.width,
        "height": args.height,
        "total_frames": total_frames,
        "speed_segments": len(schedule),
        "class_frame_count": 24,
        "classes": KERNELS,
        "timeline": "tl.txt",
        "src": "frames",
    }
    (output / "expect.json").write_text(
        json.dumps(expectation, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(
        json.dumps(
            {
                "ok": True,
                "out": str(output),
                "clips": args.clips,
                "frames_per_clip": args.frames,
                "total_frames": total_frames,
                "speed_segments": len(schedule),
            },
            separators=(",", ":"),
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
