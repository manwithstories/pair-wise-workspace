"""Generate sample clips with timestamp defects, encoded on the spot.

No external media files are used. Defects are injected either at mux time
(packet timestamp rewrite) or by MP4 sample-table surgery (stts/ctts boxes),
because libavformat muxers refuse non-monotonic DTS and PTS<DTS.
"""

from __future__ import annotations

import os
import struct
from fractions import Fraction

import av
import numpy as np

WIDTH, HEIGHT = 320, 240
FPS = 30


# ---------------------------------------------------------------- content
def make_frame(index: int, total: int) -> np.ndarray:
    """Deterministic moving-box test pattern (RGB24)."""
    fast = (index % 10) < 4
    speed = 30.0 if fast else 1.5
    x = 20 + (index * speed) % (WIDTH - 60)
    img = np.full((HEIGHT, WIDTH, 3), 40, dtype=np.uint8)
    img[80:160, int(x):int(x) + 40] = (200, 90, 60)
    yy, xx = np.mgrid[0:HEIGHT, 0:WIDTH]
    img[((yy - 120) ** 2 + (xx - 160) ** 2) < 40 ** 2] = (60, 200, 90)
    return img


def encode_clean(path: str, frames: int, gop: int = 30, bframes: int = 1,
                 codec: str = "libx264") -> None:
    """Encode a clean CFR clip; returns nothing, writes *path*."""
    container = av.open(path, "w")
    stream = container.add_stream(codec, rate=FPS)
    stream.width, stream.height = WIDTH, HEIGHT
    stream.pix_fmt = "yuv420p"
    stream.codec_context.gop_size = gop
    stream.codec_context.max_b_frames = bframes
    stream.options = (
        {"crf": "20"} if codec == "libx264" else {"x265-params": "log-level=error"}
    )
    for i in range(frames):
        frame = av.VideoFrame.from_ndarray(make_frame(i, frames), format="rgb24")
        frame = frame.reformat(format="yuv420p")
        frame.pts = i
        for packet in stream.encode(frame):
            container.mux(packet)
    for packet in stream.encode():
        container.mux(packet)
    container.close()


def remux(src: str, dst: str, modify, fmt: str | None = None) -> None:
    """Copy packets src->dst, rewriting timestamps via modify(packet, index, state)."""
    inp = av.open(src)
    ivs = inp.streams.video[0]
    out = av.open(dst, "w", format=fmt)
    ovs = out.add_stream(template=ivs)
    state = {}
    index = 0
    for packet in inp.demux(ivs):
        if packet.dts is None:
            continue
        packet.stream = ovs
        modify(packet, index, state)
        index += 1
        out.mux(packet)
    out.close()
    inp.close()


# ------------------------------------------------------- defect: drift mp4
def make_bframes_drift(tmp: str, dst: str) -> None:
    """B-frame clip with start offset, duplicate PTS and splice gaps."""
    encode_clean(tmp, frames=300, gop=30, bframes=1)
    step = 512  # one frame interval in the demuxed 1/15360 time base
    offset = 3 * 15360  # timeline starts at +3s
    gap_shifts = {90: step, 150: 2 * step, 220: step}  # cumulative splice gaps
    # Duplicate-PTS injection: pick decode-adjacent pairs exactly one frame
    # step apart, so the duplicated stamp leaves an immediately adjacent
    # hole (which the repair kernel then refills). pts >= dts is required
    # or the muxer rejects the packet.
    probe = av.open(tmp)
    stamps = [
        (p.pts, p.dts) for p in probe.demux(probe.streams.video[0]) if p.dts is not None
    ]
    probe.close()
    dup_indices = set()
    last = -2
    for i in range(1, len(stamps)):
        if len(dup_indices) >= 25:
            break
        if i in gap_shifts:
            continue  # dup pair must not straddle a splice-gap boundary
        prev_pts, (pts, dts) = stamps[i - 1][0], stamps[i]
        if i - last >= 2 and pts - prev_pts == step and prev_pts >= dts:
            dup_indices.add(i)
            last = i

    def modify(packet, i, state):
        shift = offset + sum(v for k, v in gap_shifts.items() if i >= k)
        packet.pts += shift
        packet.dts += shift
        if i in dup_indices:
            packet.pts = state.get("prev_pts", packet.pts)
        state["prev_pts"] = packet.pts

    remux(tmp, dst, modify)


# ---------------------------------------------------------- defect: vfr mkv
def make_vfr_clip(tmp: str, dst: str) -> None:
    """True VFR clip (variable frame durations) in Matroska."""
    encode_clean(tmp, frames=150, gop=30, bframes=0)
    pattern = [33, 33, 67, 17, 50, 33, 100, 33, 25, 42]

    def modify(packet, i, state):
        t = state.get("t", 0)
        packet.time_base = Fraction(1, 1000)
        packet.pts = t
        packet.dts = t
        packet.duration = pattern[i % len(pattern)]
        state["t"] = t + packet.duration

    remux(tmp, dst, modify, fmt="matroska")


# ------------------------------------------------- mp4 sample-table surgery
def _parse_boxes(data, start=0, end=None):
    end = len(data) if end is None else end
    boxes = []
    offset = start
    while offset + 8 <= end:
        size, typ = struct.unpack_from(">I4s", data, offset)
        typ = typ.decode("latin1")
        header = 8
        if size == 1:
            size = struct.unpack_from(">Q", data, offset + 8)[0]
            header = 16
        elif size == 0:
            size = end - offset
        boxes.append([typ, offset, size, header])
        offset += size
    return boxes


def _child(data, boxes, name):
    for box in boxes:
        if box[0] == name:
            return box
    raise KeyError(name)


def _stbl_chain(data):
    top = _parse_boxes(data)
    moov = _child(data, top, "moov")
    chain = [moov]
    node = moov
    for name in ("trak", "mdia", "minf", "stbl"):
        node = _child(data, _parse_boxes(data, node[1] + node[3], node[1] + node[2]), name)
        chain.append(node)
    return chain


def make_same_pts(tmp: str, dst: str) -> None:
    """Clip whose every frame carries the same PTS (via stts/ctts surgery)."""
    encode_clean(tmp, frames=60, gop=30, bframes=1)
    data = bytearray(open(tmp, "rb").read())
    # remove the edit list (its media_time no longer matches new ctts)
    top = _parse_boxes(data)
    moov = _child(data, top, "moov")
    trak = _child(data, _parse_boxes(data, moov[1] + moov[3], moov[1] + moov[2]), "trak")
    edts = _child(data, _parse_boxes(data, trak[1] + trak[3], trak[1] + trak[2]), "edts")
    data[edts[1]:edts[1] + edts[2]] = b""
    struct.pack_into(">I", data, moov[1], moov[2] - edts[2])
    struct.pack_into(">I", data, trak[1], trak[2] - edts[2])
    # stts: every sample 1 tick; ctts: N-i so every pts lands on N
    chain = _stbl_chain(data)
    stbl = chain[-1]
    children = _parse_boxes(data, stbl[1] + stbl[3], stbl[1] + stbl[2])
    stsz = _child(data, children, "stsz")
    count = struct.unpack_from(">I", data, stsz[1] + 16)[0]
    stts_payload = struct.pack(">II", 0, 1) + struct.pack(">II", count, 1)
    ctts_payload = struct.pack(">II", 0, count)
    for i in range(count):
        ctts_payload += struct.pack(">II", 1, count - i)
    edits = []
    for name, payload in (("stts", stts_payload), ("ctts", ctts_payload)):
        box = _child(data, children, name)
        edits.append((box, struct.pack(">I4s", 8 + len(payload), name.encode()) + payload))
    total = 0
    for box, new_box in sorted(edits, key=lambda e: -e[0][1]):
        data[box[1]:box[1] + box[2]] = new_box
        total += len(new_box) - box[2]
    for box in chain:
        struct.pack_into(">I", data, box[1], box[2] + total)
    open(dst, "wb").write(bytes(data))


# ------------------------------------------------------------------- driver
def generate_all(outdir: str) -> list[str]:
    """Generate the full fixture set; returns the written file paths."""
    os.makedirs(outdir, exist_ok=True)
    written = []

    def tmp(name):
        return os.path.join(outdir, f".tmp_{name}")

    def out(name):
        path = os.path.join(outdir, name)
        written.append(path)
        return path

    make_bframes_drift(tmp("drift.mp4"), out("bframes_drift.mp4"))
    make_vfr_clip(tmp("vfr.mp4"), out("vfr_clip.mkv"))
    encode_clean(out("single_frame.mp4"), frames=1, gop=30, bframes=0)
    encode_clean(out("all_intra.mp4"), frames=60, gop=1, bframes=0)
    make_same_pts(tmp("same.mp4"), out("same_pts.mp4"))
    encode_clean(tmp("tb1k.mp4"), frames=60, gop=30, bframes=1)
    remux(tmp("tb1k.mp4"), out("timebase_1k.mkv"), lambda p, i, s: None, fmt="matroska")
    encode_clean(out("hevc_clip.mp4"), frames=30, gop=30, bframes=0, codec="libx265")
    # truncated clip: valid header, missing tail
    encode_clean(tmp("trunc.mkv"), frames=90, gop=30, bframes=1)
    remux(tmp("trunc.mkv"), tmp("trunc2.mkv"), lambda p, i, s: None, fmt="matroska")
    raw = open(tmp("trunc2.mkv"), "rb").read()
    open(out("truncated.mkv"), "wb").write(raw[: len(raw) // 2])
    for name in os.listdir(outdir):
        if name.startswith(".tmp_"):
            os.remove(os.path.join(outdir, name))
    return written
