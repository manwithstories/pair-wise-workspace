"""离屏帧计划渲染器：复用模型输出逐帧 active clip 与哈希。

对每个整数帧号，在所有视频轨中找出覆盖该帧的片段，
按 (priority 大, 轨道索引小) 选出 active clip，
并按变速倍率映射出源帧号。输出帧号、clip id、源帧号与 plan 哈希。
"""
from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from fractions import Fraction

from ..timeline.model import Timeline


@dataclass
class FrameEntry:
    frame: int
    clip_id: str | None      # None 表示该帧无 active clip（空洞）
    source_frame: int | None


def _active_clip(timeline: Timeline, frame: Fraction):
    best = None
    best_key = None
    for track in timeline.video_tracks():
        for clip in track.clips:
            if clip.is_gap or clip.is_zero_length:
                continue
            if clip.range.start <= frame < clip.range.end:
                key = (track.priority, -track.index)
                if best_key is None or key > best_key:
                    best_key = key
                    best = clip
    return best


def render_plan(timeline: Timeline) -> dict:
    """生成逐帧计划：frames 列表 + 规范化 JSON 的 sha256。"""
    duration = timeline.kind_duration("video")
    frame_count = int(duration) if duration.denominator == 1 \
        else int(duration) + 1
    entries: list[FrameEntry] = []
    for frame_no in range(frame_count):
        frame = Fraction(frame_no)
        clip = _active_clip(timeline, frame)
        if clip is None:
            entries.append(FrameEntry(frame_no, None, None))
            continue
        source = clip.source_frame_at(frame)
        # 源帧号取整：变速映射误差控制在 1 帧以内
        src_int = int(source) if source >= 0 else 0
        entries.append(FrameEntry(frame_no, clip.clip_id, src_int))

    frames_payload = [
        {"frame": e.frame, "clip_id": e.clip_id, "source_frame": e.source_frame}
        for e in entries
    ]
    canonical = json.dumps(frames_payload, sort_keys=True, separators=(",", ":"))
    digest = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
    return {
        "fps": str(float(timeline.fps)),
        "frame_count": frame_count,
        "frames": frames_payload,
        "plan_sha256": digest,
    }
