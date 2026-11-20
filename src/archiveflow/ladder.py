"""Multi-bitrate ladder: aligned encoding and representation selection.

Encodes each planned segment at every target bitrate with boundaries
aligned to the shared media clock, records the achieved bitrate versus
target, and simulates delivery over a varying-bandwidth link: a
representation is selected per segment with damped switching (limited
downshift per step, sustained recovery before upshift).
"""

from __future__ import annotations

import fractions
import os
from dataclasses import dataclass, field

import av
import numpy as np

from .errors import CapabilityError
from .segment import SegmentSpec

# Target bps -> (representation name, width, height)
def geometry_for(target_bps: int) -> tuple[str, int, int]:
    if target_bps <= 700_000:
        return ("360p", 640, 360)
    if target_bps <= 1_800_000:
        return ("720p", 1280, 720)
    return ("1080p", 1920, 1080)


@dataclass(frozen=True)
class Representation:
    name: str
    target_bps: int
    width: int
    height: int


@dataclass
class SegmentResult:
    segment: SegmentSpec
    representation: Representation
    path: str  # relative file name
    actual_bps: float = 0.0
    bitrate_error_pct: float = 0.0
    selected_for_delivery: bool = False


@dataclass
class LadderResult:
    representations: list[Representation]
    segments: list[SegmentResult]
    alignment_error_ms: float = 0.0


def build_ladder(targets: list[int]) -> list[Representation]:
    reps: list[Representation] = []
    for t in sorted(targets):
        name, w, h = geometry_for(t)
        reps.append(Representation(name=name, target_bps=t, width=w,
                                   height=h))
    return reps


def encode_segment(frames: list[np.ndarray], rep: Representation,
                   fps: int, out_path: str) -> tuple[int, float]:
    """Encode RGB frames of one segment to an mp4 file.

    Returns (bytes_written, duration_s). Raises CapabilityError when the
    encoder is missing; never modifies the input frames.
    """
    try:
        av.codec.Codec("mpeg4", "w")
    except Exception as exc:
        raise CapabilityError(f"mpeg4 encoder unavailable: {exc}") from exc
    container = av.open(out_path, mode="w", format="mp4")
    try:
        stream = container.add_stream("mpeg4", rate=fps)
        stream.width, stream.height = rep.width, rep.height
        stream.pix_fmt = "yuv420p"
        stream.bit_rate = rep.target_bps
        stream.gop_size = max(1, len(frames))
        stream.options = {"threads": "1"}
        for i, arr in enumerate(frames):
            frame = av.VideoFrame.from_ndarray(arr, format="rgb24")
            frame = frame.reformat(width=rep.width, height=rep.height,
                                   format="yuv420p")
            frame.pts = i
            for pkt in stream.encode(frame):
                container.mux(pkt)
        for pkt in stream.encode(None):
            container.mux(pkt)
    finally:
        container.close()
    size = os.path.getsize(out_path)
    duration_s = len(frames) / fps if fps else 0.0
    return size, duration_s


def simulate_bandwidth(n_segments: int, seed: int = 5) -> list[float]:
    """Deterministic bandwidth trajectory with a mid-stream dip and a
    sustained recovery (bps available per segment)."""
    rng = np.random.default_rng(seed)
    bw: list[float] = []
    level = 3_000_000.0
    for i in range(n_segments):
        if n_segments >= 4 and i == n_segments // 3:
            level = 700_000.0  # sudden drop
        if i >= 2 * n_segments // 3:
            level = min(level * 1.8, 3_000_000.0)  # gradual recovery
        bw.append(level * (1 + 0.05 * rng.standard_normal()))
    return bw


def select_representations(reps: list[Representation], n_segments: int,
                           bandwidth: list[float]) -> list[int]:
    """Pick a representation index per segment with damped switching:
    at most one step down per segment; upshift only after two consecutive
    segments of bandwidth above the next tier."""
    if not reps or n_segments == 0:
        return []
    current = len(reps) - 1
    recovery_streak = 0
    choices: list[int] = []
    for i in range(n_segments):
        bw = bandwidth[i] if i < len(bandwidth) else bandwidth[-1]
        # Downshift (limited to one level) while bandwidth is short.
        if reps[current].target_bps > bw and current > 0:
            current -= 1
            recovery_streak = 0
        elif current < len(reps) - 1 \
                and reps[current + 1].target_bps <= bw * 0.9:
            recovery_streak += 1
            if recovery_streak >= 2:
                current += 1
                recovery_streak = 0
        else:
            recovery_streak = 0
        choices.append(current)
    return choices


def alignment_error_ms(results: list[SegmentResult]) -> float:
    """Max start-time divergence across representations (should be ~0
    because all layers share the planned boundaries)."""
    by_index: dict[int, list[float]] = {}
    for r in results:
        by_index.setdefault(r.segment.index, []).append(r.segment.start_ms)
    worst = 0.0
    for starts in by_index.values():
        if starts:
            worst = max(worst, max(starts) - min(starts))
    return worst
