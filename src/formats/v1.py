"""v1 扁平交换格式：仅全局起止，无 source_range / 优先级 / gap。

结构：
{
  "version": 1, "fps": "25", "name": "...",
  "tracks": [{"kind": "video", "clips": [
      {"id": "c1", "start_frame": 0, "duration_frames": 50, ...}]}]
}
未知字段透传（保留在 metadata，序列化时回写）。
"""
from __future__ import annotations

from fractions import Fraction
from typing import Any

from ..timeline.model import (
    Clip, ModelError, TimeRange, Timeline, Track, parse_fps, parse_frames,
)

_CLIP_KNOWN = {"id", "start_frame", "duration_frames"}
_TRACK_KNOWN = {"kind", "clips"}
_TOP_KNOWN = {"version", "fps", "name", "tracks"}


def _require(obj: dict, key: str, where: str) -> Any:
    if key not in obj:
        raise ModelError("MISSING_FIELD", f"{where} 缺少字段: {key}")
    return obj[key]


def parse(data: dict) -> Timeline:
    if not isinstance(data, dict):
        raise ModelError("INVALID_FORMAT", "v1 顶层必须是对象")
    fps = parse_fps(_require(data, "fps", "timeline"))
    timeline = Timeline(
        fps=fps, name=str(data.get("name", "")),
        metadata={k: v for k, v in data.items() if k not in _TOP_KNOWN},
    )
    for t_idx, t_obj in enumerate(_require(data, "tracks", "timeline")):
        kind = _require(t_obj, "kind", f"tracks[{t_idx}]")
        if kind not in ("video", "audio"):
            raise ModelError("INVALID_FIELD", f"未知轨道类型: {kind}")
        track = Track(
            kind=kind, index=t_idx, priority=t_idx,
            metadata={k: v for k, v in t_obj.items() if k not in _TRACK_KNOWN},
        )
        for c_obj in t_obj.get("clips", []):
            clip_id = str(_require(c_obj, "id", f"tracks[{t_idx}].clips"))
            start = parse_frames(_require(c_obj, "start_frame", clip_id),
                                 "start_frame")
            duration = parse_frames(
                _require(c_obj, "duration_frames", clip_id), "duration_frames")
            track.clips.append(Clip(
                clip_id=clip_id, range=TimeRange(start, duration),
                metadata={k: v for k, v in c_obj.items() if k not in _CLIP_KNOWN},
            ))
        timeline.tracks.append(track)
    return timeline


def _num(frames: Fraction) -> int | float:
    if frames.denominator == 1:
        return frames.numerator
    return float(frames)


def serialize(timeline: Timeline) -> dict:
    tracks = []
    for track in timeline.tracks:
        t_obj: dict[str, Any] = {"kind": track.kind, **track.metadata}
        clips = []
        for clip in track.sorted_clips():
            c_obj: dict[str, Any] = {
                "id": clip.clip_id,
                "start_frame": _num(clip.range.start),
                "duration_frames": _num(clip.range.duration),
                **clip.metadata,
            }
            clips.append(c_obj)
        t_obj["clips"] = clips
        tracks.append(t_obj)
    return {
        "version": 1,
        "fps": _fps_str(timeline.fps),
        "name": timeline.name,
        **timeline.metadata,
        "tracks": tracks,
    }


def _fps_str(fps: Fraction) -> str:
    for key, known in (("23.976", Fraction(24000, 1001)),
                       ("25", Fraction(25)),
                       ("29.97", Fraction(30000, 1001)),
                       ("50", Fraction(50)), ("60", Fraction(60))):
        if fps == known:
            return key
    return str(float(fps))
