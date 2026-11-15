"""扫描线重叠检测：产出 Conflict 及零长、相邻片段统计。

规则：
- 同轨重叠按半开区间判定，首尾相接不算冲突；
- 视频轨之间做跨轨检测，音频轨只做同轨检测；
- 零长片段单独计数，不参与冲突与消解。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

from .model import Clip, Timeline, Track


@dataclass
class Conflict:
    """两个片段在时间上的重叠。"""

    track_a: int          # 轨道索引（全局 tracks 下标）
    clip_a: str           # clip id
    track_b: int
    clip_b: str
    start_frame: Fraction
    end_frame: Fraction
    cross_track: bool     # True 表示跨视频轨冲突

    @property
    def overlap_frames(self) -> Fraction:
        return self.end_frame - self.start_frame


@dataclass
class ScanReport:
    conflicts: list[Conflict] = field(default_factory=list)
    zero_length_clips: list[tuple[int, str]] = field(default_factory=list)  # (track, clip)
    adjacent_pairs: list[tuple[int, str, str]] = field(default_factory=list)

    @property
    def conflict_count(self) -> int:
        return len(self.conflicts)

    @property
    def zero_length_count(self) -> int:
        return len(self.zero_length_clips)


def _live_clips(track: Track) -> list[Clip]:
    """参与冲突判定的片段：非零长、非 gap。"""
    return [c for c in track.sorted_clips() if not c.is_zero_length and not c.is_gap]


def _sweep_pairs(clips: list[Clip]):
    """扫描线：对按起点排序的片段产出所有重叠对 (clip_a, clip_b)。"""
    active: list[Clip] = []
    for clip in clips:
        active = [c for c in active if c.range.end > clip.range.start]
        for other in active:
            yield other, clip
        active.append(clip)


def scan_timeline(timeline: Timeline) -> ScanReport:
    report = ScanReport()

    # 同轨统计：零长、相邻、同轨冲突
    for t_idx, track in enumerate(timeline.tracks):
        ordered = track.sorted_clips()
        for clip in ordered:
            if clip.is_zero_length:
                report.zero_length_clips.append((t_idx, clip.clip_id))
        live = _live_clips(track)
        for prev, nxt in zip(live, live[1:]):
            if prev.range.end == nxt.range.start:
                report.adjacent_pairs.append((t_idx, prev.clip_id, nxt.clip_id))
        for a, b in _sweep_pairs(live):
            report.conflicts.append(Conflict(
                track_a=t_idx, clip_a=a.clip_id,
                track_b=t_idx, clip_b=b.clip_id,
                start_frame=max(a.range.start, b.range.start),
                end_frame=min(a.range.end, b.range.end),
                cross_track=False,
            ))

    # 跨视频轨冲突：合并各视频轨片段后扫描线
    video = timeline.video_tracks()
    if len(video) > 1:
        entries: list[tuple[int, Clip]] = []
        for track in video:
            g_idx = timeline.tracks.index(track)
            for clip in _live_clips(track):
                entries.append((g_idx, clip))
        entries.sort(key=lambda e: (e[1].range.start, e[0], e[1].clip_id))
        active: list[tuple[int, Clip]] = []
        for g_idx, clip in entries:
            active = [e for e in active if e[1].range.end > clip.range.start]
            for o_idx, other in active:
                if o_idx == g_idx:
                    continue  # 同轨冲突已在上面统计
                report.conflicts.append(Conflict(
                    track_a=o_idx, clip_a=other.clip_id,
                    track_b=g_idx, clip_b=clip.clip_id,
                    start_frame=max(other.range.start, clip.range.start),
                    end_frame=min(other.range.end, clip.range.end),
                    cross_track=True,
                ))
            active.append((g_idx, clip))
    return report


def audio_video_deviation(timeline: Timeline) -> Fraction:
    """音频总长与视频总长的偏差（帧），取各音频轨最大偏差。"""
    video_end = timeline.kind_duration("video")
    deviation = Fraction(0)
    for track in timeline.audio_tracks():
        end = Fraction(0)
        for clip in track.clips:
            if clip.range.end > end:
                end = clip.range.end
        deviation = max(deviation, abs(end - video_end))
    return deviation
