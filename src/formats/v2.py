"""v2 交换格式：在 v1 基础上增加 source_range、轨道优先级与显式 gap。

新增字段：
- track.priority: int，数值大者跨轨胜出；
- clip.source_range: {"start_frame": N, "duration_frames": M}；
- clip.speed: "0.5" | "1" | "2" | "3"（字符串或数字）；
- clip.type: "gap" 表示显式 gap。
v1 升 v2 时源区间按全长补齐（start=0, duration=片段时长），
priority 缺省按轨道索引，未知字段透传。
"""
from __future__ import annotations

from fractions import Fraction
from typing import Any

from ..timeline.model import (
    Clip, ModelError, TimeRange, Timeline, Track, parse_fps, parse_frames,
)
from . import v1

_CLIP_KNOWN = {"id", "start_frame", "duration_frames", "source_range",
               "speed", "type"}
_TRACK_KNOWN = {"kind", "clips", "priority"}
_TOP_KNOWN = {"version", "fps", "name", "tracks"}


def _require(obj: dict, key: str, where: str) -> Any:
    if key not in obj:
        raise ModelError("MISSING_FIELD", f"{where} 缺少字段: {key}")
    return obj[key]


def _parse_speed(value: Any, clip_id: str) -> Fraction:
    try:
        speed = Fraction(str(value))
    except (ValueError, ZeroDivisionError) as exc:
        raise ModelError("INVALID_FIELD",
                         f"{clip_id} 的 speed 非法: {value!r}") from exc
    if speed <= 0:
        raise ModelError("INVALID_FIELD", f"{clip_id} 的 speed 必须为正")
    return speed


def parse(data: dict) -> Timeline:
    if not isinstance(data, dict):
        raise ModelError("INVALID_FORMAT", "v2 顶层必须是对象")
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
            kind=kind, index=t_idx,
            priority=int(t_obj.get("priority", t_idx)),
            metadata={k: v for k, v in t_obj.items() if k not in _TRACK_KNOWN},
        )
        for c_obj in t_obj.get("clips", []):
            clip_id = str(_require(c_obj, "id", f"tracks[{t_idx}].clips"))
            start = parse_frames(_require(c_obj, "start_frame", clip_id),
                                 "start_frame")
            duration = parse_frames(
                _require(c_obj, "duration_frames", clip_id), "duration_frames")
            source = None
            if "source_range" in c_obj:
                s_obj = c_obj["source_range"]
                source = TimeRange(
                    start=parse_frames(s_obj.get("start_frame", 0),
                                       "source_range.start_frame"),
                    duration=parse_frames(
                        _require(s_obj, "duration_frames", clip_id),
                        "source_range.duration_frames"))
            track.clips.append(Clip(
                clip_id=clip_id, range=TimeRange(start, duration),
                source_range=source,
                speed=_parse_speed(c_obj.get("speed", 1), clip_id),
                is_gap=c_obj.get("type") == "gap",
                metadata={k: v for k, v in c_obj.items() if k not in _CLIP_KNOWN},
            ))
        timeline.tracks.append(track)
    return timeline


def serialize(timeline: Timeline) -> dict:
    tracks = []
    for track in timeline.tracks:
        t_obj: dict[str, Any] = {"kind": track.kind, "priority": track.priority,
                                 **track.metadata}
        clips = []
        for clip in track.sorted_clips():
            c_obj: dict[str, Any] = {
                "id": clip.clip_id,
                "start_frame": v1._num(clip.range.start),
                "duration_frames": v1._num(clip.range.duration),
            }
            if clip.source_range is not None:
                c_obj["source_range"] = {
                    "start_frame": v1._num(clip.source_range.start),
                    "duration_frames": v1._num(clip.source_range.duration),
                }
            if clip.speed != 1:
                c_obj["speed"] = v1._num(clip.speed)
            if clip.is_gap:
                c_obj["type"] = "gap"
            c_obj.update(clip.metadata)
            clips.append(c_obj)
        t_obj["clips"] = clips
        tracks.append(t_obj)
    return {"version": 2, "fps": v1._fps_str(timeline.fps),
            "name": timeline.name, **timeline.metadata, "tracks": tracks}


def normalize_v1(data: dict) -> dict:
    """v1 升 v2：源区间按全长补齐，priority 按轨道索引，未知字段透传。"""
    timeline = v1.parse(data)
    for track in timeline.tracks:
        for clip in track.clips:
            if clip.source_range is None:
                clip.source_range = TimeRange(Fraction(0), clip.range.duration)
    return serialize(timeline)
