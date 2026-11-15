#!/usr/bin/env python3
"""Generate a synthetic YUV420p fixture with moving gradients and a block."""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))

from abrl.media.yuv import Frame, write_frames  # noqa: E402


def _parse_size(text: str) -> tuple[int, int]:
    width, height = (int(part) for part in text.lower().split("x", 1))
    if width % 2 or height % 2:
        raise argparse.ArgumentTypeError("size must use even width and height")
    return width, height


def make_frame(index: int, width: int, height: int, rng: np.random.Generator) -> Frame:
    cols, rows = np.meshgrid(np.arange(width), np.arange(height))
    phase = index * 0.06
    # High-frequency texture: expensive to code, keeps quality gains
    # available at high bitrates so ladders can climb into the Mbps range.
    texture = 16.0 * np.sin(cols / 3.0 + 0.4 * np.sin(phase)) * np.sin(rows / 3.7)
    luma = (
        128.0
        + 30.0 * np.sin((cols + 10.0 * np.sin(phase)) / 40.0)
        + 18.0 * np.cos((rows + 7.0 * np.cos(phase)) / 32.0)
        + texture
    )
    block_x = int((width * 0.15 + index * 0.8) % max(width - 1, 1))
    block_y = int(height * 0.25 + 6 * np.sin(phase)) % max(height - 1, 1)
    bw, bh = max(8, width // 8), max(8, height // 8)
    luma[block_y:min(block_y + bh, height), block_x:min(block_x + bw, width)] += 28
    luma += rng.normal(0, 2.0, luma.shape)
    y = np.clip(luma, 0, 255).astype(np.uint8)
    u = np.clip(
        128 + 18 * np.sin(cols[::2, ::2] / 52.0 + phase)
        + rng.normal(0, 1.0, (height // 2, width // 2)), 0, 255
    ).astype(np.uint8)
    v = np.clip(
        128 + 15 * np.cos(rows[::2, ::2] / 44.0 - phase)
        + rng.normal(0, 1.0, (height // 2, width // 2)), 0, 255
    ).astype(np.uint8)
    return Frame(y, u, v)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate synthetic YUV420p test material")
    parser.add_argument("--out", required=True)
    parser.add_argument("--frames", type=int, default=96)
    parser.add_argument("--size", type=_parse_size, default="640x360")
    parser.add_argument("--seed", type=int, default=7)
    args = parser.parse_args(argv)
    width, height = args.size
    rng = np.random.default_rng(args.seed)
    frames = [make_frame(i, width, height, rng) for i in range(args.frames)]
    written = write_frames(args.out, frames)
    meta = Path(args.out).with_suffix(".meta.json")
    meta.write_text(json.dumps(
        {"width": width, "height": height, "frames": args.frames, "seed": args.seed}, indent=2),
        encoding="utf-8")
    print(f"wrote {args.frames} frames {width}x{height} -> {args.out} ({written} bytes)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
