"""I/P/B distributions and keyframe interval summaries."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable

from .frame_source import FrameRecord


@dataclass(frozen=True)
class FrameKindSummary:
    i_frames: int
    p_frames: int
    b_frames: int
    other_frames: int
    keyframe_intervals: list[int]

    @property
    def has_b_frames(self) -> bool:
        return self.b_frames > 0

    def to_dict(self) -> dict[str, object]:
        return {
            "i_frames": self.i_frames,
            "p_frames": self.p_frames,
            "b_frames": self.b_frames,
            "other_frames": self.other_frames,
            "has_b_frames": self.has_b_frames,
            "keyframe_intervals": self.keyframe_intervals,
        }


def summarize_frame_kinds(frames: Iterable[FrameRecord]) -> FrameKindSummary:
    items = list(frames)
    counts = {"I": 0, "P": 0, "B": 0}
    keyframe_positions = []
    for item in items:
        if item.pict_type in counts:
            counts[item.pict_type] += 1
        if item.keyframe or item.pict_type == "I":
            keyframe_positions.append(item.frame_index)
    intervals = [
        right - left for left, right in zip(keyframe_positions, keyframe_positions[1:])
    ]
    return FrameKindSummary(
        i_frames=counts["I"],
        p_frames=counts["P"],
        b_frames=counts["B"],
        other_frames=len(items) - sum(counts.values()),
        keyframe_intervals=intervals,
    )
