"""固定块混音引擎:离线与流式两条执行路径共享的核心。

BlockEngine 逐块驱动全部轨道(重采样 -> 自动化 -> 增益/映射 -> 求和),
运算顺序与块大小无关:相同输入在任意 block_frames 下产出位级一致
的 float64 总线。离线与流式仅替换源实现与块大小。
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .automation import AutomationCurve
from .graph import place_track, render_track_block
from .metrics import MetricsAccumulator, TrackMetrics
from .resample import Resampler
from .sources import StreamSource
from .types import (
    MixBlock,
    ProjectConfig,
    TrackStat,
    TrackSpec,
)

_READ_CHUNK = 4096


@dataclass
class EngineResult:
    """引擎产物:总线块序列 + 主/轨统计 + best-effort 跳过记录。"""

    blocks: list[MixBlock]
    master: MetricsAccumulator
    track_stats: list[TrackStat]
    skipped: list[TrackStat]
    automation_samples: int
    project_frames: int


class _Runtime:
    """单轨运行时:源、重采样器、曲线、游标与混音前统计。"""

    def __init__(self, spec: TrackSpec, source: StreamSource, project_rate: int,
                 clip_threshold: float) -> None:
        self.spec = spec
        self.source = source
        self.resampler = Resampler(source.sample_rate, project_rate)
        self.curve = AutomationCurve(spec.automation)
        self.pending = np.zeros((0, source.channels), dtype=np.float64)
        self.exhausted = False
        self.finished = False
        self.produced = 0
        self.target_frames = self.resampler.output_frames_for(source.frames)
        self.metrics = TrackMetrics(clip_threshold)
        self.stat = TrackStat(
            name=spec.name,
            source_sample_rate=source.sample_rate,
            source_channels=source.channels,
            source_frames=source.frames,
            project_frames=self.target_frames,
        )

    def _fill_pending(self, need: int) -> None:
        while not self.exhausted and self.pending.shape[0] < need:
            raw = self.source.read(_READ_CHUNK)
            if raw.shape[0] == 0:
                self.exhausted = True
                tail = self.resampler.flush()
                if tail.shape[0]:
                    self.pending = np.concatenate([self.pending, tail], axis=0)
                break
            out = self.resampler.feed(raw)
            if out.shape[0]:
                self.pending = np.concatenate([self.pending, out], axis=0)

    def take(self, frames: int) -> np.ndarray:
        """顺序消费轨道本地时间轴上的项目帧,不足右侧由调用方补零。"""

        if self.finished:
            return np.zeros((0, self.pending.shape[1]), dtype=np.float64)
        self._fill_pending(frames)
        take_n = min(frames, self.pending.shape[0], self.target_frames - self.produced)
        if take_n <= 0:
            if self.produced >= self.target_frames:
                self.finished = True
            return np.zeros((0, self.pending.shape[1]), dtype=np.float64)
        part = self.pending[:take_n]
        self.pending = self.pending[take_n:]
        self.produced += take_n
        if self.produced >= self.target_frames:
            self.finished = True
        return part


class BlockEngine:
    """逐块驱动全部轨道并求和,产出 (frames, 2) 总线块。"""

    def __init__(self, config: ProjectConfig, sources: dict[str, StreamSource]) -> None:
        self.config = config
        self.runtimes: list[_Runtime] = []
        self.skipped: list[TrackStat] = []
        for spec in config.tracks:
            source = sources.get(spec.name)
            if source is None:
                # best-effort 下坏轨已在 build_sources 记录并从 sources 排除,
                # 引擎直接跳过;fail-fast 不会出现缺源(build_sources 已抛错)
                continue
            self.runtimes.append(
                _Runtime(spec, source, config.sample_rate, config.clip_threshold)
            )
        self.master = MetricsAccumulator(config.clip_threshold)
        self._automation_samples = sum(len(s.automation) for s in config.tracks)

    def run(self, block_frames: int) -> EngineResult:
        blocks: list[MixBlock] = []
        total = self.config.duration_frames
        index = 0
        for start in range(0, total, block_frames):
            size = min(block_frames, total - start)
            block_buf = np.zeros((size, 2), dtype=np.float64)
            for rt in self.runtimes:
                self._render_into(rt, start, size, block_buf)
            blocks.append(MixBlock(index=index, start=start, frames=size,
                                   buffer=block_buf))
            self.master.add(block_buf)
            index += 1
        return EngineResult(
            blocks=blocks,
            master=self.master,
            track_stats=[rt.stat for rt in self.runtimes],
            skipped=self.skipped,
            automation_samples=self._automation_samples,
            project_frames=total,
        )

    def _render_into(self, rt: _Runtime, block_start: int, size: int,
                     block_buf: np.ndarray) -> None:
        spec = rt.spec
        # 轨道本地时间轴上,本块覆盖 [local_start, local_end)
        local_start = block_start - spec.offset
        local_end = local_start + size
        if local_end <= 0 or local_start >= rt.target_frames:
            return  # 整块在素材之前或之后,零贡献

        # 顺序消费:先跳过本块起点之前尚未取走的帧(偏移/块边界对齐)
        while rt.produced < local_start and not rt.finished:
            if rt.take(local_start - rt.produced).shape[0] == 0:
                break
        src_end = min(local_end, rt.target_frames)
        want = src_end - rt.produced
        if want <= 0:
            return
        part = rt.take(want)
        if part.shape[0] == 0:
            return

        positions = np.arange(rt.produced - part.shape[0], rt.produced, dtype=np.int64)
        curve_gain = rt.curve.sample(positions)
        contribution = render_track_block(spec, part, curve_gain)
        rt.metrics.add(contribution)
        rt.stat.peak = rt.metrics.peak
        rt.stat.clipped_samples = rt.metrics.clipped_samples
        first_local = rt.produced - part.shape[0]
        place_track(block_buf, contribution, spec.offset + first_local - block_start)

    def close(self) -> None:
        for rt in self.runtimes:
            rt.source.close()
