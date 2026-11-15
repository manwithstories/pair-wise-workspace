"""Synthetic fixture generation (no external media is ever referenced).

Four clips are produced with NumPy-generated moving patterns and encoded
with libx264 at 320x180, 24 fps, time base 1/12288, 360 frames:

* ``clean_bframes``    - regular I/P/B cadence, perfectly spaced pts
* ``bpyramid_gap``     - B-pyramid GOPs, six local jitters and one gap
* ``vfr_jitter``       - cumulative variable-frame-rate style jitter
* ``truncated_tail``   - valid clip whose tail bytes are cut off

Anomalies are injected as display-order pts values fed to the encoder.
Local jitters are single +256-tick bumps whose surrounding deltas stay
positive, and the gap is a permanent +1024-tick tail shift so packet dts
remains monotone for the MP4 muxer while the display axis still reports
one large, individually repairable jump.
"""

from __future__ import annotations

from fractions import Fraction
from pathlib import Path

import av
import numpy as np

FIXTURE_FPS = 24
FIXTURE_WIDTH = 320
FIXTURE_HEIGHT = 180
FIXTURE_TIME_BASE = Fraction(1, 12288)
FIXTURE_FRAME_COUNT = 360
GOP_SIZE = 12
B_FRAMES = 3
STEP_TICKS = 12288 // FIXTURE_FPS  # 512
JITTER_TICKS = 256
GAP_TICKS = 1024
GAP_ANCHOR = 300

FIXTURE_NAMES = (
    "clean_bframes",
    "bpyramid_gap",
    "vfr_jitter",
)


def _synthetic_frame(index: int, rng: np.random.Generator) -> np.ndarray:
    """RGB uint8 frame: moving gradient band plus bounded noise."""

    image = np.zeros((FIXTURE_HEIGHT, FIXTURE_WIDTH, 3), dtype=np.uint8)
    offset = (index * 7) % FIXTURE_WIDTH
    image[:, offset:, 0] = np.linspace(
        0, 255, FIXTURE_WIDTH - offset, dtype=np.uint8
    )[None, :]
    image[:, :, 1] = np.linspace(
        0, 255, FIXTURE_HEIGHT, dtype=np.uint8
    )[:, None]
    band = index % FIXTURE_HEIGHT
    image[band:band + 24, :, 2] = 200
    noisy = image.astype(np.uint16) + rng.integers(
        0, 24, (FIXTURE_HEIGHT, FIXTURE_WIDTH, 1), dtype=np.uint16
    )
    return noisy.clip(0, 255).astype(np.uint8)


def _encoder_options() -> dict[str, str]:
    return {
        "bf": str(B_FRAMES),
        "bpyramid": "1",
        "gop": str(GOP_SIZE),
        "keyint_min": str(GOP_SIZE),
        "sc_threshold": "0",
        "b_adapt": "0",
        "preset": "medium",
    }


def _open_encoder(container: av.container.OutputContainer) -> av.video.stream.VideoStream:
    stream = container.add_stream(
        "libx264", rate=FIXTURE_FPS, options=_encoder_options()
    )
    stream.width = FIXTURE_WIDTH
    stream.height = FIXTURE_HEIGHT
    stream.pix_fmt = "yuv420p"
    stream.time_base = FIXTURE_TIME_BASE
    return stream


def bpyramid_pts(frame_count: int, seed: int) -> list[int]:
    """Display pts with six local jitters and one permanent tail gap."""

    rng = np.random.default_rng(seed + 101)
    pts = [index * STEP_TICKS for index in range(frame_count)]
    anchors = list(range(28, 200, (200 - 28) // 6))[:6]
    for anchor in anchors:
        pts[anchor] += JITTER_TICKS
    for index in range(GAP_ANCHOR, frame_count):
        pts[index] += GAP_TICKS
    return pts


def vfr_pts(frame_count: int, seed: int) -> list[int]:
    """Cumulative non-decreasing walk producing variable positive gaps."""

    rng = np.random.default_rng(seed + 202)
    running = 0
    pts: list[int] = []
    for index in range(frame_count):
        if index and index % 9 == 0:
            running += int(rng.choice((JITTER_TICKS, 512, 768)))
        pts.append(index * STEP_TICKS + running)
    return pts


def _encode(path: Path, pts: list[int], seed: int) -> None:
    rng = np.random.default_rng(seed)
    container = av.open(
        str(path), "w", options={"movflags": "+faststart"}
    )
    try:
        stream = _open_encoder(container)
        for index, frame_pts in enumerate(pts):
            frame = av.VideoFrame.from_ndarray(
                _synthetic_frame(index, rng), format="rgb24"
            )
            frame.pts = int(frame_pts)
            if index and index % GOP_SIZE == 0:
                frame.pict_type = av.video.frame.PictureType.I
            for packet in stream.encode(frame):
                container.mux(packet)
        for packet in stream.encode():
            container.mux(packet)
    finally:
        container.close()


def generate_fixtures(out_dir: str | Path, seed: int = 7) -> dict[str, Path]:
    """Generate all four fixtures below *out_dir*; return their paths."""

    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    names = (*FIXTURE_NAMES, "truncated_tail")
    paths = {name: out / f"{name}.mp4" for name in names}

    clean_pts = [index * STEP_TICKS for index in range(FIXTURE_FRAME_COUNT)]
    _encode(paths["clean_bframes"], clean_pts, seed)
    _encode(
        paths["bpyramid_gap"],
        bpyramid_pts(FIXTURE_FRAME_COUNT, seed),
        seed,
    )
    _encode(
        paths["vfr_jitter"],
        vfr_pts(FIXTURE_FRAME_COUNT, seed),
        seed,
    )

    _encode(paths["truncated_tail"], clean_pts, seed)
    data = paths["truncated_tail"].read_bytes()
    paths["truncated_tail"].write_bytes(data[: int(len(data) * 0.82)])
    return paths
