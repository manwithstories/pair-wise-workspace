"""显式时间线与轨道依赖图。

依赖图表达机位覆盖区间与音轨映射关系：缺失依赖、循环依赖在此拦截；
全局时间线把各轨的独立时钟（sync_offset_us + 修复后 PTS）对齐到统一微秒轴，
并汇总显式缺口。下游调度只能沿拓扑边提交。
"""
from __future__ import annotations

from dataclasses import dataclass, field

from .model import (ErrorCategory, FrameRecord, Issue, Manifest,
                    PipelineError, TrackDecl, TrackKind, TrackResult)


@dataclass(slots=True)
class TimelineSegment:
    track_id: str
    kind: TrackKind
    start_us: int            # 已含 sync_offset
    end_us: int
    has_gap: bool = False


@dataclass(slots=True)
class Gap:
    track_id: str
    start_us: int
    end_us: int


@dataclass(slots=True)
class Timeline:
    manifest: Manifest
    order: list[str]                       # 拓扑序
    segments: list[TimelineSegment]
    gaps: list[Gap] = field(default_factory=list)
    issues: list[Issue] = field(default_factory=list)

    def duration_us(self) -> int:
        return max((s.end_us for s in self.segments), default=0)

    def video_duration_us(self) -> int:
        return max((s.end_us for s in self.segments
                    if s.kind is TrackKind.VIDEO), default=0)


# ---------------------------------------------------------------- 依赖图

def topological_order(manifest: Manifest) -> list[str]:
    """Kahn 拓扑排序；缺失依赖在 probe.validate_manifest 已拦，此处查环。"""
    indeg = {tid: 0 for tid in manifest.tracks}
    adj: dict[str, list[str]] = {tid: [] for tid in manifest.tracks}
    for tid, tr in manifest.tracks.items():
        for dep in tr.depends_on:
            adj[dep].append(tid)
            indeg[tid] += 1

    ready = sorted(t for t, d in indeg.items() if d == 0)
    order: list[str] = []
    while ready:
        cur = ready.pop(0)
        order.append(cur)
        for nxt in sorted(adj[cur]):
            indeg[nxt] -= 1
            if indeg[nxt] == 0:
                ready.append(nxt)
        ready.sort()

    if len(order) != len(manifest.tracks):
        cyclic = [t for t, d in indeg.items() if d > 0]
        raise PipelineError(
            ErrorCategory.DEPENDENCY_CYCLE,
            f"检测到循环依赖，涉及轨道: {', '.join(sorted(cyclic))}")
    return order


def blocked_by_failure(track: TrackDecl,
                       results: dict[str, TrackResult]) -> str | None:
    """上游失败/跳过则返回首个阻断源，否则 None。"""
    for dep in track.depends_on:
        r = results.get(dep)
        if r is None or r.state.value in ("failed", "skipped"):
            return dep
    return None


# ---------------------------------------------------------------- 时间线构建

def build_timeline(manifest: Manifest,
                   frame_index: dict[str, list[FrameRecord]],
                   audio_durations: dict[str, int]) -> Timeline:
    order = topological_order(manifest)
    segments: list[TimelineSegment] = []
    gaps: list[Gap] = []

    for tid in order:
        tr = manifest.tracks[tid]
        off = tr.sync_offset_us
        if tr.kind is TrackKind.VIDEO:
            frames = frame_index.get(tid, [])
            if not frames:
                continue
            start = off + frames[0].pts_us
            end = off + frames[-1].pts_us
            has_gap = any(f.is_gap for f in frames)
            for f in frames:
                if f.is_gap:
                    # 单帧缺口用一帧时长闭合区间。
                    frame_us = round(1_000_000 / tr.frame_rate)
                    gaps.append(Gap(tid, off + f.pts_us,
                                    off + f.pts_us + frame_us))
        else:
            dur = audio_durations.get(tid, 0)
            start, end, has_gap = off, off + dur, False
        segments.append(TimelineSegment(tid, tr.kind, start, end, has_gap))

    return Timeline(manifest=manifest, order=order, segments=segments,
                    gaps=gaps)


# ---------------------------------------------------------------- 同步误差

def sync_error_ms(manifest: Manifest,
                  frame_index: dict[str, list[FrameRecord]],
                  audio_results: dict[str, TrackResult]) -> float:
    """以参考视频为基准，比较各音频轨声明偏移与实际覆盖起点的偏差（毫秒）。

    音频实际起点取其 sync_offset_us；视频首帧取修复后 PTS。
    返回所有跨轨对齐检查点的最大绝对偏差。
    """
    ref_id = manifest.reference_video
    if not ref_id or ref_id not in frame_index:
        return 0.0
    ref_start = frame_index[ref_id][0].pts_us if frame_index[ref_id] else 0
    worst = 0.0
    for tid, tr in manifest.tracks.items():
        if tr.kind is not TrackKind.AUDIO or tid not in audio_results:
            continue
        # 期望起点 = 参考起点 + 声明偏移；实际起点同为偏移轴，
        # 偏差来自缺口/补齐导致的覆盖漂移，用音频结果记录的边界补齐换算。
        ar = audio_results[tid]
        drift_us = 0
        if ar.audio is not None:
            drift_us = round(ar.audio.padded_samples * 1_000_000
                             / max(1, ar.audio.target_rate))
        expected = ref_start + tr.sync_offset_us
        actual = ref_start + tr.sync_offset_us + drift_us
        worst = max(worst, abs(actual - expected) / 1000.0)
    return worst
