"""Aggregate frame/GOP statistics from reordered display-order frames."""

from __future__ import annotations

from dataclasses import dataclass, asdict
from fractions import Fraction

from .reorder import ReorderResult, ReorderedFrame


@dataclass
class FrameStats:
    decoded_frames: int
    i_frames: int
    p_frames: int
    b_frames: int
    other_frames: int
    gop_count: int
    max_gop_size: int
    gop_sizes: list[int]
    dropped_packets: int
    order_violations: int
    inferred_pts: int
    window_peak: int
    nominal_fps: float | None
    measured_fps: float | None
    time_base: str
    first_pts: int | None
    last_pts: int | None
    first_time_ms: float | None
    last_time_ms: float | None

    def as_dict(self) -> dict:
        return asdict(self)


def _gop_sizes(frames: list[ReorderedFrame]) -> list[int]:
    sizes: list[int] = []
    current = 0
    for frame in frames:
        if frame.keyframe or frame.pict_type == "I":
            if current:
                sizes.append(current)
            current = 1
        else:
            current += 1
    if current:
        sizes.append(current)
    return sizes


def build_stats(
    reordered: ReorderResult,
    time_base: Fraction,
    nominal_rate: Fraction | None,
    dropped_packets: int,
    declared_fps: Fraction | None = None,
) -> FrameStats:
    """Compute counts, GOP layout and measured frame rate."""

    frames = reordered.frames
    counts = {"I": 0, "P": 0, "B": 0}
    other = 0
    for frame in frames:
        if frame.pict_type in counts:
            counts[frame.pict_type] += 1
        else:
            other += 1

    gops = _gop_sizes(frames)
    fps_source = declared_fps or nominal_rate
    nominal_fps = float(fps_source) if fps_source else None

    measured_fps: float | None = None
    if len(frames) >= 2 and frames[-1].pts > frames[0].pts:
        span_seconds = (frames[-1].pts - frames[0].pts) * float(time_base)
        if span_seconds > 0:
            measured_fps = (len(frames) - 1) / span_seconds

    first_pts = frames[0].pts if frames else None
    last_pts = frames[-1].pts if frames else None
    return FrameStats(
        decoded_frames=len(frames),
        i_frames=counts["I"],
        p_frames=counts["P"],
        b_frames=counts["B"],
        other_frames=other,
        gop_count=len(gops),
        max_gop_size=max(gops, default=0),
        gop_sizes=gops,
        dropped_packets=dropped_packets,
        order_violations=reordered.order_violations,
        inferred_pts=reordered.inferred_pts,
        window_peak=reordered.window_peak,
        nominal_fps=nominal_fps,
        measured_fps=measured_fps,
        time_base=f"{time_base.numerator}/{time_base.denominator}",
        first_pts=first_pts,
        last_pts=last_pts,
        first_time_ms=(first_pts * float(time_base) * 1000.0
                       if first_pts is not None else None),
        last_time_ms=(last_pts * float(time_base) * 1000.0
                      if last_pts is not None else None),
    )
