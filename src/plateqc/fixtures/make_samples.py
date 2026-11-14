"""Generate synthetic plate samples with PyAV (no external files).

Samples:
  plate_bframes_normal.mp4   clean B-frame GOP structure
  plate_bframes_broken.mp4   duplicate/regressed/gapped timestamps
  plate_truncated.mp4        tail truncated mid-stream
  plate_all_i.mp4            all intra frames

Timestamps are authored at time_base 1/30000 (1000 units per frame at
30 fps); the MP4 muxer rescales them losslessly to its own timescale.
"""

from __future__ import annotations

import argparse
import os
from fractions import Fraction

import av
import numpy as np

WIDTH = 160
HEIGHT = 96
FPS = 30
STEP = 1000  # pts units per frame at TB
TB = Fraction(1, 30000)


def _frame_array(index: int, seed: int) -> np.ndarray:
    col = np.arange(WIDTH, dtype=np.uint32)[None, :]
    row = np.arange(HEIGHT, dtype=np.uint32)[:, None]
    red = (col + index * 2 + seed) % 256
    green = (row + index + seed) % 256
    blue = ((col + row) // 2 + index + seed) % 256
    return np.stack(
        [
            np.broadcast_to(red, (HEIGHT, WIDTH)),
            np.broadcast_to(green, (HEIGHT, WIDTH)),
            np.broadcast_to(blue, (HEIGHT, WIDTH)),
        ],
        axis=-1,
    ).astype(np.uint8)


def _encode_segment(nframes: int, bf: int, keyint: int, seed: int):
    """Encode one closed GOP segment; returns (packets, open container)."""
    import io

    buf = io.BytesIO()
    container = av.open(buf, "w", format="mp4")
    stream = container.add_stream("libx264", rate=FPS)
    stream.width, stream.height = WIDTH, HEIGHT
    stream.pix_fmt = "yuv420p"
    stream.time_base = TB
    stream.options = {
        "bf": str(bf),
        "x264-params": f"b-adapt=0:scenecut=0:keyint={keyint}:keyint-min={keyint}",
        "crf": "23",
    }
    for i in range(nframes):
        frame = av.video.frame.VideoFrame.from_ndarray(
            _frame_array(i, seed), format="rgb24"
        )
        frame.pts = i * STEP
        for packet in stream.encode(frame):
            container.mux(packet)
    for packet in stream.encode(None):
        container.mux(packet)
    container.close()
    buf.seek(0)
    demux = av.open(buf)
    video = demux.streams.video[0]
    packets = [
        (bytes(packet), packet.pts // STEP, packet.is_keyframe)
        for packet in demux.demux(video)
        if packet.size > 0
    ]
    return packets, demux


def build_file(path, segments, corrupt=None, faststart=False, truncate_ratio=None):
    """Mux encoded segments into one MP4 with fully controlled timestamps."""
    corrupt = corrupt or {}
    all_packets = []
    holders = []
    start = 0
    for idx, (nframes, bf, keyint) in enumerate(segments):
        packets, demux = _encode_segment(nframes, bf, keyint, seed=idx * 37 + 11)
        holders.append(demux)
        for raw, local_display, is_key in packets:
            all_packets.append((raw, start + local_display, is_key))
        start += nframes

    delay = max(
        [0] + [dec - disp for dec, (_, disp, _) in enumerate(all_packets)]
    )
    pts_values = [int(corrupt.get(disp, disp * STEP)) for _, disp, _ in all_packets]
    # Keep pts >= dts for the muxer: shift the whole DTS ladder down by a
    # constant margin covering the deepest corrupted dip.
    margin = max(
        [0]
        + [
            (dec - delay) * STEP - pts + STEP
            for dec, pts in enumerate(pts_values)
        ]
    )
    options = {"movflags": "faststart"} if faststart else None
    out = av.open(path, "w", format="mp4", options=options)
    out_stream = out.add_stream(template=holders[0].streams.video[0])
    for dec, (raw, disp, is_key) in enumerate(all_packets):
        packet = av.packet.Packet(raw)
        packet.stream = out_stream
        packet.time_base = TB
        packet.pts = pts_values[dec]
        packet.dts = (dec - delay) * STEP - margin
        packet.is_keyframe = is_key
        out.mux(packet)
    out.close()
    for demux in holders:
        demux.close()

    if truncate_ratio is not None:
        with open(path, "rb") as handle:
            data = handle.read()
        with open(path, "wb") as handle:
            handle.write(data[: int(len(data) * truncate_ratio)])
    return len(all_packets)


def _broken_corruption() -> dict:
    """Corrupted display-order pts (units of STEP=1000), 37 repairs total.

    A pts outlier relocates in display order, so anomalies are authored to
    keep every other frame on the nominal grid:
      5 duplicates (pts[i] == pts[i-1])         -> 1 repair each =  5
      1 regression 0.6 frame back (double-flag) -> 2 repairs
      splice hole at frame 570: the whole tail  -> 30 repairs
      is shifted +25275 units (842.5 ms)
    """
    table = {}
    for i in (20, 60, 100, 140, 180):
        table[i] = (i - 1) * STEP
    table[40] = 39 * STEP - 600
    for j in range(570, 600):
        table[j] = j * STEP + 25275
    return table


def make_all(out_dir: str) -> list:
    os.makedirs(out_dir, exist_ok=True)
    made = []

    path = os.path.join(out_dir, "plate_bframes_normal.mp4")
    build_file(path, [(300, 2, 100)])
    made.append(path)

    path = os.path.join(out_dir, "plate_bframes_broken.mp4")
    segments = [(109, 2, 1000)] * 5 + [(55, 3, 1000)]
    build_file(path, segments, corrupt=_broken_corruption())
    made.append(path)

    path = os.path.join(out_dir, "plate_truncated.mp4")
    build_file(path, [(400, 2, 100)], faststart=True, truncate_ratio=0.55)
    made.append(path)

    path = os.path.join(out_dir, "plate_all_i.mp4")
    build_file(path, [(120, 0, 1)])
    made.append(path)
    return made


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(prog="make_samples")
    parser.add_argument("--out", required=True, help="output directory")
    args = parser.parse_args(argv)
    for path in make_all(args.out):
        print(f"wrote {path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
