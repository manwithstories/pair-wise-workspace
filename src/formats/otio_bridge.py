"""自研模型与 OpenTimelineIO 的双向转换。

映射关系：
- Timeline.fps -> 各 RationalTime 的 rate（float 近似，帧值保持有理数整数）；
- Track -> otio.schema.Track（kind 映射 Video/Audio），priority 存 metadata；
- Clip -> otio.schema.Clip / Gap，source_range 映射同名区间，
  speed 通过 source_range 时长 = 时间线时长 * speed 表达。
"""
from __future__ import annotations

from fractions import Fraction

import opentimelineio as otio

from ..timeline.model import Clip, TimeRange, Timeline, Track

_KIND_TO_OTIO = {"video": otio.schema.TrackKind.Video,
                 "audio": otio.schema.TrackKind.Audio}
_KIND_FROM_OTIO = {v: k for k, v in _KIND_TO_OTIO.items()}


def _rt(frames: Fraction, fps: Fraction) -> otio.opentime.RationalTime:
    return otio.opentime.RationalTime(value=float(frames), rate=float(fps))


def _tr(rng: TimeRange, fps: Fraction) -> otio.opentime.TimeRange:
    return otio.opentime.TimeRange(start_time=_rt(rng.start, fps),
                                   duration=_rt(rng.duration, fps))


def to_otio(timeline: Timeline) -> otio.schema.Timeline:
    ot = otio.schema.Timeline(name=timeline.name or "timeline")
    ot.metadata["model"] = dict(timeline.metadata)
    ot.metadata["fps"] = str(float(timeline.fps))
    for track in timeline.tracks:
        ot_track = otio.schema.Track(name=f"{track.kind}{track.index}",
                                     kind=_KIND_TO_OTIO[track.kind])
        ot_track.metadata["priority"] = track.priority
        ot_track.metadata["model"] = dict(track.metadata)
        for clip in track.sorted_clips():
            if clip.is_gap:
                item = otio.schema.Gap(name=clip.clip_id)
                item.source_range = _tr(clip.range, timeline.fps)
            else:
                item = otio.schema.Clip(name=clip.clip_id)
                src = clip.source_range or TimeRange(Fraction(0),
                                                     clip.range.duration)
                item.source_range = _tr(src, timeline.fps)
                item.metadata["model"] = {
                    "start_frame": str(clip.range.start),
                    "duration_frames": str(clip.range.duration),
                    "speed": str(clip.speed),
                    **{k: v for k, v in clip.metadata.items()},
                }
            ot_track.append(item)
        ot.tracks.append(ot_track)
    return ot


def from_otio(ot: otio.schema.Timeline) -> Timeline:
    fps = Fraction(str(ot.metadata.get("fps", "25"))).limit_denominator(1001)
    timeline = Timeline(fps=fps, name=ot.name or "",
                        metadata=dict(ot.metadata.get("model", {})))
    for t_idx, ot_track in enumerate(ot.tracks):
        kind = _KIND_FROM_OTIO.get(ot_track.kind, "video")
        track = Track(kind=kind, index=t_idx,
                      priority=int(ot_track.metadata.get("priority", t_idx)),
                      metadata=dict(ot_track.metadata.get("model", {})))
        cursor = Fraction(0)  # OTIO 轨道为顺序排列，用游标累计时间线位置
        for item in ot_track:
            duration = Fraction(item.duration().value).limit_denominator(10**9)
            if isinstance(item, otio.schema.Gap):
                track.clips.append(Clip(
                    clip_id=item.name or f"gap_{cursor}",
                    range=TimeRange(cursor, duration), is_gap=True))
                cursor += duration
                continue
            meta = dict(item.metadata.get("model", {}))
            start = Fraction(meta.pop("start_frame", str(cursor)))
            tl_dur = Fraction(meta.pop("duration_frames", str(duration)))
            speed = Fraction(meta.pop("speed", "1"))
            src = item.source_range
            source = TimeRange(Fraction(src.start_time.value),
                               Fraction(src.duration.value)) if src else None
            track.clips.append(Clip(
                clip_id=item.name, range=TimeRange(start, tl_dur),
                source_range=source, speed=speed, metadata=meta))
            cursor = start + tl_dur
        timeline.tracks.append(track)
    return timeline


def round_trip_equal(timeline: Timeline) -> tuple[bool, list[str]]:
    """导出 OTIO 再读回，逐条比对区间，返回 (是否等价, 差异描述)。"""
    back = from_otio(to_otio(timeline))
    diffs: list[str] = []
    if len(back.tracks) != len(timeline.tracks):
        return False, [f"轨道数不等: {len(back.tracks)} != {len(timeline.tracks)}"]
    for t_a, t_b in zip(timeline.tracks, back.tracks):
        ca, cb = t_a.sorted_clips(), t_b.sorted_clips()
        if len(ca) != len(cb):
            diffs.append(f"轨道{t_a.index} 片段数不等: {len(ca)} != {len(cb)}")
            continue
        for x, y in zip(ca, cb):
            if x.clip_id != y.clip_id:
                diffs.append(f"clip id 不等: {x.clip_id} != {y.clip_id}")
            if x.range.start != y.range.start or \
                    x.range.duration != y.range.duration:
                diffs.append(
                    f"{x.clip_id} 区间不等: "
                    f"[{x.range.start},{x.range.end}) != "
                    f"[{y.range.start},{y.range.end})")
    return (not diffs), diffs
