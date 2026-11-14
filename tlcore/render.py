"""Frame-layer routing and deterministic frame-map hashing."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any

from .model import Timeline
from .timebase import NS_PER_SECOND, frames_for_duration, parse_rate


@dataclass(slots=True)
class FrameLayer:
    track: str
    clip_id: str
    layer: int
    local_frame: int
    source_frame: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "track": self.track,
            "clip_id": self.clip_id,
            "layer": self.layer,
            "local_frame": self.local_frame,
            "source_frame": self.source_frame,
        }


@dataclass(slots=True)
class RenderResult:
    fps: Fraction
    frames: int
    layers_max: int
    sha256: str
    frame_map: list[list[FrameLayer]] = field(default_factory=list)

    def summary(self) -> dict[str, Any]:
        return {
            "fps": {"num": self.fps.numerator, "den": self.fps.denominator},
            "frames": self.frames,
            "layers_max": self.layers_max,
            "sha256": self.sha256,
        }


def _ceil_frame(seconds: Fraction, fps: Fraction) -> int:
    value = seconds * fps
    return -(-value.numerator // value.denominator)


def _floor_frame(seconds: Fraction, fps: Fraction) -> int:
    value = seconds * fps
    return value.numerator // value.denominator


def build_frame_map(timeline: Timeline, fps: Fraction | int | str | None = None
                    ) -> list[list[FrameLayer]]:
    """Build frames as a list of ordered layer entries.

    Clip intervals are half-open on the timeline.  A frame belongs to a clip
    when its exact frame time lies in the interval; local_frame starts at zero
    for the first visible frame of each occurrence.
    """
    rate = timeline.fps if fps is None else (fps if isinstance(fps, Fraction) else parse_rate(fps))
    duration_ns = timeline.computed_duration_ns
    total_frames = frames_for_duration(duration_ns, rate)
    frames: list[list[FrameLayer]] = [[] for _ in range(total_frames)]
    for track_index, track in enumerate(timeline.tracks):
        for clip in track.ordered_clips():
            if clip.duration_ns <= 0:
                continue
            start_sec = Fraction(clip.start_ns, NS_PER_SECOND)
            end_sec = Fraction(clip.end_ns, NS_PER_SECOND)
            first = _ceil_frame(start_sec, rate)
            stop = _ceil_frame(end_sec, rate)
            first = min(first, total_frames)
            stop = min(stop, total_frames)
            source_start = _floor_frame(Fraction(clip.source_in_ns, NS_PER_SECOND), rate)
            for frame_index in range(first, stop):
                frames[frame_index].append(FrameLayer(
                    track=track.id,
                    clip_id=clip.id,
                    layer=0,
                    local_frame=frame_index - first,
                    source_frame=source_start + frame_index - first,
                ))
    for entries in frames:
        for layer_index, entry in enumerate(entries):
            entry.layer = layer_index
    return frames


def canonical_frame_bytes(frame_map: list[list[FrameLayer]]) -> bytes:
    lines: list[str] = []
    for frame_index, entries in enumerate(frame_map):
        lines.append(json.dumps(
            {"frame": frame_index, "layers": [entry.to_dict() for entry in entries]},
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ))
    return ("\n".join(lines) + ("\n" if lines else "")).encode("utf-8")


def hash_frame_map(frame_map: list[list[FrameLayer]]) -> str:
    return hashlib.sha256(canonical_frame_bytes(frame_map)).hexdigest()


def render_timeline(timeline: Timeline, fps: Fraction | int | str | None = None
                    ) -> RenderResult:
    rate = timeline.fps if fps is None else (fps if isinstance(fps, Fraction) else parse_rate(fps))
    frame_map = build_frame_map(timeline, rate)
    layers_max = max((len(entries) for entries in frame_map), default=0)
    return RenderResult(
        fps=rate,
        frames=len(frame_map),
        layers_max=layers_max,
        sha256=hash_frame_map(frame_map),
        frame_map=frame_map,
    )


render = render_timeline
generate_frame_map = build_frame_map
