"""重叠消解器：按优先级与裁剪策略迭代消解至不动点。

规则：
- 跨视频轨冲突：轨道 priority 数值大者胜出，同级按轨道索引小者胜出；
- 同轨冲突：起点早者胜出，并列按 clip id 字典序；
- 音频轨只做同轨检测，不参与消解（遗留冲突由 CLI 以退出码 1 报告）；
- 被压片段按重叠位置裁剪头/尾、中间劈开或整段让位，并记录原因；
- 每轮消解后重扫，裁剪可能产生新的零长或相邻片段，迭代至不动点；
- 超过 MAX_ROUNDS 轮报 NON_CONVERGENCE。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

from .model import Clip, ModelError, TimeRange, Timeline
from .scan import Conflict, scan_timeline

MAX_ROUNDS = 32


@dataclass
class Resolution:
    clip_id: str
    action: str            # trim_head | trim_tail | split | drop
    reason: str
    track: int


@dataclass
class ResolveResult:
    timeline: Timeline
    rounds: int
    resolutions: list[Resolution] = field(default_factory=list)
    conflicts_detected: int = 0
    conflicts_resolved: int = 0
    zero_length_clips: int = 0


def _winner_key(timeline: Timeline, track_idx: int) -> tuple:
    """跨轨胜出键：priority 大者胜，并列轨道索引小者胜。"""
    return (-timeline.tracks[track_idx].priority, track_idx)


def _pick_winner(timeline: Timeline, conflict: Conflict,
                 a: Clip, b: Clip) -> tuple[int, Clip, Clip]:
    """返回 (负方轨道索引, 胜方, 负方)。"""
    if conflict.cross_track:
        if _winner_key(timeline, conflict.track_a) <= \
                _winner_key(timeline, conflict.track_b):
            return conflict.track_b, a, b
        return conflict.track_a, b, a
    # 同轨：起点早者胜，并列按 clip id
    if (a.range.start, a.clip_id) <= (b.range.start, b.clip_id):
        return conflict.track_b, a, b
    return conflict.track_a, b, a


def _shift_source(clip: Clip, timeline_delta: Fraction) -> None:
    """裁剪头部时同步平移源区间（考虑变速倍率）。"""
    if clip.source_range is not None:
        clip.source_range = TimeRange(
            start=clip.source_range.start + timeline_delta * clip.speed,
            duration=clip.source_range.duration - timeline_delta * clip.speed)


def _apply_resolution(timeline: Timeline, conflict: Conflict,
                      index: dict[str, tuple[int, Clip]],
                      resolutions: list[Resolution]) -> bool:
    """对单条冲突执行一次消解，返回是否改写了模型。"""
    entry_a = index.get(conflict.clip_a)
    entry_b = index.get(conflict.clip_b)
    if entry_a is None or entry_b is None:
        return False
    _, a = entry_a
    _, b = entry_b
    if not a.range.overlaps(b.range):
        return False

    l_idx, winner, loser = _pick_winner(timeline, conflict, a, b)
    track = timeline.tracks[l_idx]
    ws, we = winner.range.start, winner.range.end
    ls, le = loser.range.start, loser.range.end

    def record(action: str, reason: str) -> None:
        resolutions.append(Resolution(loser.clip_id, action, reason, l_idx))

    if ws <= ls and we >= le:
        track.clips.remove(loser)
        index.pop(loser.clip_id, None)
        record("drop", f"被 {winner.clip_id} 完全覆盖，整段让位")
        return True
    if ws <= ls < we < le:
        delta = we - ls
        loser.range = TimeRange(start=we, duration=le - we)
        _shift_source(loser, delta)
        record("trim_head", f"头部与 {winner.clip_id} 重叠 {delta} 帧，裁剪让位")
        return True
    if ls < ws < le <= we:
        loser.range = TimeRange(start=ls, duration=ws - ls)
        if loser.source_range is not None:
            loser.source_range = TimeRange(
                start=loser.source_range.start,
                duration=loser.range.duration * loser.speed)
        record("trim_tail", f"尾部与 {winner.clip_id} 重叠 {le - ws} 帧，裁剪让位")
        return True
    if ls < ws and we < le:
        right = Clip(
            clip_id=f"{loser.clip_id}#2",
            range=TimeRange(start=we, duration=le - we),
            source_range=None if loser.source_range is None else TimeRange(
                start=loser.source_range.start + (we - ls) * loser.speed,
                duration=(le - we) * loser.speed),
            speed=loser.speed, is_gap=loser.is_gap,
            metadata=dict(loser.metadata))
        loser.range = TimeRange(start=ls, duration=ws - ls)
        if loser.source_range is not None:
            loser.source_range = TimeRange(
                start=loser.source_range.start,
                duration=loser.range.duration * loser.speed)
        track.clips.append(right)
        index[right.clip_id] = (l_idx, right)
        record("split", f"中段被 {winner.clip_id} 压住，劈为两段")
        return True
    return False


def _build_index(timeline: Timeline) -> dict[str, tuple[int, Clip]]:
    return {c.clip_id: (t.index, c)
            for t in timeline.tracks for c in t.clips}


def resolve_timeline(timeline: Timeline, policy: str = "priority") -> ResolveResult:
    """迭代消解至不动点。policy 目前支持 "priority"。"""
    if policy != "priority":
        raise ModelError("INVALID_POLICY", f"未知消解策略: {policy}")
    result = ResolveResult(timeline=timeline, rounds=0)
    audio_idx = {t.index for t in timeline.audio_tracks()}

    for round_no in range(1, MAX_ROUNDS + 1):
        report = scan_timeline(timeline)
        result.rounds = round_no
        if round_no == 1:
            result.conflicts_detected = report.conflict_count
        # 音频轨冲突只检测不消解
        todo = [c for c in report.conflicts
                if c.track_a not in audio_idx and c.track_b not in audio_idx]
        if not todo:
            result.zero_length_clips = report.zero_length_count
            return result
        index = _build_index(timeline)
        for conflict in todo:
            if _apply_resolution(timeline, conflict, index,
                                 result.resolutions):
                result.conflicts_resolved += 1
    raise ModelError("NON_CONVERGENCE", f"消解超过 {MAX_ROUNDS} 轮未收敛")
