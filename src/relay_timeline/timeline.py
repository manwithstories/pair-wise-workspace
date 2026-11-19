"""时间线：多轨合并、剪辑点校验、硬切与定长转场。

所有位置与时长均为整数刻度（见 timebase.py）。轨道内剪辑不得重叠；
轨道间按 track 序号叠放，序号大者在上。剪辑可携带定长淡入转场，
与下方可见内容做线性混合。缺帧填补策略由渲染层按 fill_policy 执行。
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .errors import TimelineError
from .timebase import TimeBase

FILL_BLACK = "fill"
FILL_ABORT = "abort"
FILL_POLICIES = (FILL_BLACK, FILL_ABORT)


@dataclass(frozen=True)
class Clip:
    """一个剪辑：把 source_id 的素材放到时间线 [start, start+duration)。"""

    track: int
    start: int
    duration: int
    source_id: str
    source_offset: int = 0
    transition_in: int = 0  # 头部淡入时长（刻度），0 表示硬切

    @property
    def end(self) -> int:
        return self.start + self.duration


@dataclass(frozen=True)
class Layer:
    """某一时刻参与合成的一层。"""

    source_id: str
    source_ticks: int
    weight: float


@dataclass
class Timeline:
    tb: TimeBase
    duration: int
    clips: list[Clip] = field(default_factory=list)
    fill_policy: str = FILL_BLACK

    def validate(self) -> None:
        self.tb.require_duration(self.duration, "时间线时长")
        if self.duration == 0:
            raise TimelineError("时间线时长不能为零")
        if self.fill_policy not in FILL_POLICIES:
            raise TimelineError(f"未知缺帧策略: {self.fill_policy}")
        by_track: dict[int, list[Clip]] = {}
        for clip in self.clips:
            self._validate_clip(clip)
            by_track.setdefault(clip.track, []).append(clip)
        for track, clips in by_track.items():
            clips.sort(key=lambda c: c.start)
            for prev, cur in zip(clips, clips[1:]):
                if cur.start < prev.end:
                    raise TimelineError(
                        f"轨道 {track} 剪辑重叠: "
                        f"[{prev.start},{prev.end}) 与 [{cur.start},{cur.end})"
                    )

    def _validate_clip(self, clip: Clip) -> None:
        if clip.track < 0:
            raise TimelineError(f"轨道序号不能为负: {clip.track}")
        self.tb.require_duration(clip.duration, "剪辑时长")
        if clip.duration == 0:
            raise TimelineError(f"剪辑时长不能为零: {clip.source_id}")
        if clip.start < 0:
            raise TimelineError(f"剪辑起点越界: {clip.start}")
        if clip.end > self.duration:
            raise TimelineError(
                f"剪辑点越界: [{clip.start},{clip.end}) 超出时间线 {self.duration}"
            )
        self.tb.require_duration(clip.source_offset, "素材偏移")
        self.tb.require_duration(clip.transition_in, "转场时长")
        if clip.transition_in > clip.duration:
            raise TimelineError(
                f"转场时长 {clip.transition_in} 超过剪辑时长 {clip.duration}"
            )

    def composition_at(self, ticks: int) -> list[Layer]:
        """返回某时刻的合成层（自下而上），权重归一。"""
        if ticks < 0 or ticks >= self.duration:
            raise TimelineError(f"合成时刻越界: {ticks}")
        layers: list[Layer] = []
        for clip in self.clips:
            if not (clip.start <= ticks < clip.end):
                continue
            local = ticks - clip.start
            weight = 1.0
            if clip.transition_in > 0 and local < clip.transition_in:
                weight = local / clip.transition_in
            layers.append(
                Layer(
                    source_id=clip.source_id,
                    source_ticks=clip.source_offset + local,
                    weight=weight,
                )
            )
        if not layers:
            return []
        layers.sort(key=lambda l: self._track_of(l.source_id, ticks))
        # 顶层权重不足 1 时，把剩余权重按比例分给下层（转场混合）
        top = layers[-1]
        if top.weight >= 1.0:
            return [top]
        remainder = 1.0 - top.weight
        lower = layers[:-1]
        if lower:
            scale = remainder / sum(l.weight for l in lower)
            lower = [Layer(l.source_id, l.source_ticks, l.weight * scale) for l in lower]
        return lower + [top]

    def _track_of(self, source_id: str, ticks: int) -> int:
        for clip in self.clips:
            if clip.source_id == source_id and clip.start <= ticks < clip.end:
                return clip.track
        return -1

    def source_ranges(self) -> dict[str, list[tuple[int, int]]]:
        """各素材被引用的来源区间（闭区间起点、开区间终点，刻度）。"""
        ranges: dict[str, list[tuple[int, int]]] = {}
        for clip in self.clips:
            ranges.setdefault(clip.source_id, []).append(
                (clip.source_offset, clip.source_offset + clip.duration)
            )
        return ranges
