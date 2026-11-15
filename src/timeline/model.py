"""自研时间线轨道模型：全流程唯一事实源。

所有时间量统一用 Fraction 表示的"帧号"（相对时间线帧率），
区间为半开区间 [start, start + duration)。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any

# 支持的帧率（含 NTSC 分数帧率），全部以 Fraction 精确表示
SUPPORTED_FPS: dict[str, Fraction] = {
    "23.976": Fraction(24000, 1001),
    "25": Fraction(25, 1),
    "29.97": Fraction(30000, 1001),
    "50": Fraction(50, 1),
    "60": Fraction(60, 1),
}


class ModelError(Exception):
    """结构化模型错误，携带错误码，CLI 层映射为退出码 2。"""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


def parse_fps(value: Any) -> Fraction:
    """把字符串/数字帧率解析为 Fraction，未知帧率报 FPS_CONFLICT。"""
    if isinstance(value, Fraction):
        return value
    key = str(value)
    if key in SUPPORTED_FPS:
        return SUPPORTED_FPS[key]
    try:
        fps = Fraction(key).limit_denominator(1001)
    except (ValueError, ZeroDivisionError) as exc:
        raise ModelError("FPS_CONFLICT", f"无法解析帧率: {value!r}") from exc
    for known in SUPPORTED_FPS.values():
        if abs(float(fps - known)) * (1 / float(known)) < 1e-6:
            return known
    raise ModelError("FPS_CONFLICT", f"不支持的帧率: {value!r}")


def parse_frames(value: Any, field_name: str) -> Fraction:
    """解析帧号/帧数，要求为非负有理数。"""
    try:
        frames = Fraction(value)
    except (TypeError, ValueError, ZeroDivisionError) as exc:
        raise ModelError("INVALID_FIELD", f"{field_name} 不是合法数值: {value!r}") from exc
    if frames < 0:
        raise ModelError("NEGATIVE_DURATION", f"{field_name} 不允许为负: {value!r}")
    return frames


@dataclass
class TimeRange:
    """半开区间 [start, start + duration)，单位为帧。"""

    start: Fraction
    duration: Fraction

    @property
    def end(self) -> Fraction:
        return self.start + self.duration

    def overlaps(self, other: "TimeRange") -> bool:
        """半开区间相交判定：首尾相接（end == start）不算重叠。"""
        return self.start < other.end and other.start < self.end

    def overlap_len(self, other: "TimeRange") -> Fraction:
        if not self.overlaps(other):
            return Fraction(0)
        return min(self.end, other.end) - max(self.start, other.start)


@dataclass
class Clip:
    """一个片段：时间线上的摆放区间 + 源素材区间 + 变速倍率。"""

    clip_id: str
    range: TimeRange                      # 时间线区间（时间线帧率下的帧）
    source_range: TimeRange | None = None  # 源素材区间（源帧率下的帧）
    speed: Fraction = Fraction(1)          # 播放倍率：0.5x / 1x / 2x / 3x
    is_gap: bool = False                   # 显式 gap（v2）
    metadata: dict[str, Any] = field(default_factory=dict)

    @property
    def is_zero_length(self) -> bool:
        return self.range.duration == 0

    def source_frame_at(self, timeline_frame: Fraction) -> Fraction:
        """时间线帧号 -> 源素材帧号（考虑变速与裁剪偏移）。"""
        if self.source_range is None:
            return Fraction(0)
        offset = (timeline_frame - self.range.start) * self.speed
        return self.source_range.start + offset


@dataclass
class Track:
    kind: str                              # "video" | "audio"
    index: int
    priority: int = 0                      # 数值大者胜出（跨视频轨消解）
    clips: list[Clip] = field(default_factory=list)
    metadata: dict[str, Any] = field(default_factory=dict)

    def sorted_clips(self) -> list[Clip]:
        return sorted(self.clips, key=lambda c: (c.range.start, c.clip_id))


@dataclass
class Timeline:
    fps: Fraction
    tracks: list[Track] = field(default_factory=list)
    name: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)

    @property
    def duration(self) -> Fraction:
        """时间线总时长（帧）：所有轨道的最大结束帧。"""
        end = Fraction(0)
        for track in self.tracks:
            for clip in track.clips:
                if clip.range.end > end:
                    end = clip.range.end
        return end

    def kind_duration(self, kind: str) -> Fraction:
        end = Fraction(0)
        for track in self.tracks:
            if track.kind != kind:
                continue
            for clip in track.clips:
                if clip.range.end > end:
                    end = clip.range.end
        return end

    def video_tracks(self) -> list[Track]:
        return [t for t in self.tracks if t.kind == "video"]

    def audio_tracks(self) -> list[Track]:
        return [t for t in self.tracks if t.kind == "audio"]

    def clip_count(self) -> int:
        return sum(len(t.clips) for t in self.tracks)


def frames_to_seconds(frames: Fraction, fps: Fraction) -> Fraction:
    """帧数转秒，有理数精确换算。"""
    return frames / fps


def seconds_to_frames(seconds: Fraction, fps: Fraction) -> Fraction:
    """秒转帧数；调用方负责按需要取整，保证换算误差不超过 1 帧。"""
    return seconds * fps
