"""Unified per-frame timeline shared by terminal and JSONL outputs."""

from __future__ import annotations

from dataclasses import asdict, dataclass
from fractions import Fraction
from typing import Optional

from .decode import FrameInfo
from .repair import RepairResult, ticks_to_ms


@dataclass(slots=True)
class TimelineFrame:
    display_index: int
    decode_index: int
    packet_index: int
    pict_type: str
    keyframe: bool
    source_pts: Optional[int]
    source_pts_ms: Optional[int]
    repaired_pts: int
    repaired_pts_ms: int
    action: str
    anomaly: Optional[str]
    synthetic: bool
    packet_size: int
    decode_error: Optional[str]

    def to_dict(self) -> dict[str, object]:
        return asdict(self)


def build_timeline(
    frames: list[FrameInfo], repair: RepairResult, time_base: Fraction
) -> list[TimelineFrame]:
    if len(frames) != len(repair.decisions):
        raise ValueError("repair decisions and presentation frames differ in length")
    timeline: list[TimelineFrame] = []
    for display_index, (frame, decision) in enumerate(zip(frames, repair.decisions)):
        source_pts = frame.pts
        timeline.append(
            TimelineFrame(
                display_index=display_index,
                decode_index=frame.decode_index,
                packet_index=frame.packet_index,
                pict_type=frame.pict_type,
                keyframe=frame.keyframe,
                source_pts=source_pts,
                source_pts_ms=(
                    ticks_to_ms(source_pts, time_base) if source_pts is not None else None
                ),
                repaired_pts=decision.repaired_pts,
                repaired_pts_ms=ticks_to_ms(decision.repaired_pts, time_base),
                action=decision.action,
                anomaly=decision.anomaly,
                synthetic=frame.synthetic,
                packet_size=frame.packet_size,
                decode_error=frame.decode_error,
            )
        )
    return timeline


def timeline_stats(timeline: list[TimelineFrame]) -> dict[str, object]:
    pict_type = {"I": 0, "P": 0, "B": 0, "other": 0}
    for frame in timeline:
        pict_type[frame.pict_type] = pict_type.get(frame.pict_type, 0) + 1
    return {
        "decoded_frames": len(timeline),
        "pict_type": pict_type,
        "keyframes": sum(frame.keyframe for frame in timeline),
    }
