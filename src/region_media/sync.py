"""音画对齐评估与修正策略，支持可变帧率（VFR）视频。

不以固定帧率假设帧时间，而是逐帧使用实际 PTS 映射到音频公共时间线
（见 :mod:`region_media.clock`），与同时刻的音频包基准比较得到每帧
音画偏差；输出最大/P95 偏差以及每帧的修正动作（拉伸/收缩/丢帧/
重复帧），合成恢复包不参与同步评分，避免恢复手段污染同步指标。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .clock import ClockDomain
from .model import PacketModel, TrackKind

# |偏差| 超过该阈值才生成修正动作（us）
CORRECT_THRESHOLD_US = 20_000
# 单帧修正量上限，超过则建议丢帧/重复帧而非微调
FRAME_STEP_LIMIT_US = 40_000
# 标称 30fps 帧周期（us），VFR 干净间隔过滤的参考上限
NOMINAL_FRAME_US = 33_334


@dataclass
class SyncFrame:
    video_seq: int
    common_pts_us: int
    audio_ref_us: int
    offset_ms: float
    action: str  # hold | stretch | shrink | drop | duplicate


@dataclass
class SyncReport:
    samples: int = 0
    max_abs_ms: float = 0.0
    p95_abs_ms: float = 0.0
    mean_ms: float = 0.0
    actions: dict[str, int] = field(default_factory=dict)
    vfr: bool = False
    frames: list[SyncFrame] = field(default_factory=list)


class SyncAnalyzer:
    def __init__(self, domain: ClockDomain) -> None:
        self._domain = domain
        self._audio_pts: list[int] = []
        # (全局 seq, 原始 pts)，用于过滤丢包/恢复区后的 VFR 判定
        self._video_frames: list[tuple[int, int]] = []
        self._report = SyncReport()

    def feed(self, packets: list[PacketModel]) -> None:
        for pkt in packets:
            if pkt.track == TrackKind.AUDIO and not pkt.synthetic:
                self._audio_pts.append(pkt.pts_us)
        if not self._audio_pts:
            return
        audio = np.asarray(self._audio_pts, dtype=np.int64)
        for pkt in packets:
            if pkt.track != TrackKind.VIDEO or pkt.synthetic:
                continue
            common = self._domain.to_common(TrackKind.VIDEO, pkt.pts_us)
            self._video_frames.append((pkt.seq, pkt.pts_us))
            # 最近邻音频基准：VFR 下帧间隔不均，不能按帧号推算
            idx = int(np.searchsorted(audio, common))
            ref_idx = min(max(idx, 0), len(audio) - 1)
            if 0 < idx < len(audio):
                left, right = audio[idx - 1], audio[idx]
                ref_idx = idx - 1 if common - left <= right - common else idx
            audio_ref = int(audio[ref_idx])
            offset_us = common - audio_ref
            action = self._action(offset_us, pkt.duration_us)
            frame = SyncFrame(pkt.seq, common, audio_ref,
                              round(offset_us / 1000.0, 3), action)
            self._report.frames.append(frame)
            self._report.actions[action] = \
                self._report.actions.get(action, 0) + 1

    @staticmethod
    def _action(offset_us: int, frame_dur_us: int) -> str:
        if abs(offset_us) <= CORRECT_THRESHOLD_US:
            return "hold"
        if abs(offset_us) > FRAME_STEP_LIMIT_US:
            return "drop" if offset_us > 0 else "duplicate"
        return "shrink" if offset_us > 0 else "stretch"

    def finalize(self) -> SyncReport:
        frames = self._report.frames
        if not frames:
            return self._report
        offsets = np.abs(np.asarray([f.offset_ms for f in frames]))
        raw = np.asarray([f.offset_ms for f in frames])
        self._report.samples = len(frames)
        self._report.max_abs_ms = round(float(offsets.max()), 3)
        self._report.p95_abs_ms = round(float(np.percentile(offsets, 95)), 3)
        self._report.mean_ms = round(float(raw.mean()), 3)
        # VFR 判定基于视频帧「原始 PTS 间隔」，且只采纳全局序号连续、
        # 间隔落在标称帧周期合理倍数内（<=3 倍）的相邻帧——丢包、损坏
        # 分片跳过会产生整秒级异常间隔，属于传输损伤而非源帧率变化。
        clean_deltas: list[float] = []
        for i in range(1, len(self._video_frames)):
            prev_seq, prev_pts = self._video_frames[i - 1]
            seq, pts = self._video_frames[i]
            delta = pts - prev_pts
            if seq == prev_seq + 1 and 0 < delta <= NOMINAL_FRAME_US * 3:
                clean_deltas.append(delta)
        if len(clean_deltas) > 10:
            arr = np.asarray(clean_deltas, dtype=np.float64)
            mean = arr.mean()
            cv = float(arr.std() / mean) if mean > 0 else 0.0
            self._report.vfr = bool(cv > 0.08)
        else:
            # 干净连续帧不足（如损坏/强丢包场景），不具备 VFR 判定条件
            self._report.vfr = False
        return self._report
