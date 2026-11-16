#!/usr/bin/env python3
"""Generate deterministic local media fixtures with PyAV encoders."""

from __future__ import annotations

import argparse
from pathlib import Path

import av
import numpy as np

WIDTH = 160
HEIGHT = 90
FPS = 25
CASES = ("bframes", "bad_pts", "no_pts", "truncated", "no_b")


def _frame(index: int) -> av.VideoFrame:
    y, x = np.mgrid[0:HEIGHT, 0:WIDTH]
    band = ((x + index * 4) % 256).astype(np.uint8)
    stripe = (((y // 8) + index) % 2 * 24).astype(np.uint8)
    rgb = np.stack((band, np.roll(band, index % WIDTH), band // 2 + stripe), axis=-1)
    return av.VideoFrame.from_ndarray(np.ascontiguousarray(rgb), format="rgb24")


def _encode(container: av.container.OutputContainer, frames: int, bframes: int, gop: int) -> None:
    stream = container.add_stream("libx264", rate=FPS)
    stream.width = WIDTH
    stream.height = HEIGHT
    stream.pix_fmt = "yuv420p"
    stream.options = {
        "g": str(gop),
        "keyint_min": str(gop),
        "bf": str(bframes),
        "sc_threshold": "0",
        "crf": "20",
    }
    for index in range(frames):
        for packet in stream.encode(_frame(index)):
            container.mux(packet)
    for packet in stream.encode():
        container.mux(packet)


def _write_mp4(path: Path, frames: int, bframes: int, gop: int = 30) -> None:
    container = av.open(str(path), "w", format="mp4")
    try:
        _encode(container, frames, bframes, gop)
    finally:
        container.close()


def _write_bad_pts(source: Path, target: Path) -> None:
    source_container = av.open(str(source))
    output = av.open(str(target), "w", format="mp4")
    try:
        source_stream = source_container.streams.video[0]
        output_stream = output.add_stream(template=source_stream)
        offset = int(round(2.5 / source_stream.time_base))
        index = 0
        for packet in source_container.demux(source_stream):
            if packet.size == 0 or packet.dts is None:
                continue
            copied = av.Packet(packet)
            copied.stream = output_stream
            copied.time_base = packet.time_base
            copied.pts = (packet.pts or 0) + offset
            copied.dts = packet.dts + offset
            if index > 0 and index % 6 == 0 and copied.pts != copied.dts:
                copied.pts = copied.dts
            output.mux(copied)
            index += 1
    finally:
        output.close()
        source_container.close()


def _write_raw_h264(source: Path, target: Path) -> None:
    with av.open(str(source)) as container:
        stream = container.streams.video[0]
        bitstream = av.BitStreamFilterContext("h264_mp4toannexb", stream, stream)
        with target.open("wb") as output:
            output.write(stream.codec_context.extradata)
            for packet in container.demux(stream):
                if packet.size == 0 or packet.dts is None:
                    continue
                for annexb in bitstream.filter(packet):
                    output.write(bytes(annexb))


def _write_truncated(path: Path, full_path: Path) -> None:
    container = av.open(
        str(full_path),
        "w",
        format="mp4",
        options={"movflags": "frag_keyframe+empty_moov"},
    )
    try:
        _encode(container, 120, 2, 12)
    finally:
        container.close()
    payload = full_path.read_bytes()
    path.write_bytes(payload[: int(len(payload) * 0.76)])
    full_path.unlink()


def generate(output_dir: Path, cases: int) -> list[Path]:
    output_dir.mkdir(parents=True, exist_ok=True)
    paths: list[Path] = []
    base = output_dir / "bframes.mp4"
    if cases >= 1:
        _write_mp4(base, 240, 2)
        paths.append(base)
    if cases >= 2:
        target = output_dir / "bad_pts.mp4"
        _write_bad_pts(base, target)
        paths.append(target)
    if cases >= 3:
        target = output_dir / "no_pts.mp4"
        _write_raw_h264(base, target)
        paths.append(target)
    if cases >= 4:
        target = output_dir / "truncated.mp4"
        _write_truncated(target, output_dir / ".truncated-full.mp4")
        paths.append(target)
    if cases >= 5:
        target = output_dir / "no_b.mp4"
        _write_mp4(target, 120, 0)
        paths.append(target)
    return paths


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, default=Path("fixtures"))
    parser.add_argument("--cases", type=int, default=len(CASES), choices=range(1, len(CASES) + 1))
    args = parser.parse_args()
    for path in generate(args.out, args.cases):
        print(path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
