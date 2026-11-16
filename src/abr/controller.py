"""模拟控制器：编排轨迹、估计器、缓冲模拟与策略决策。

控制器持有跨段的滞后状态（连续安全段数、冷却计数、当前档位），每段依次
执行「估计 -> 决策 -> 下载 -> 回填样本 -> 累计指标」。所有随机来源仅
存在于轨迹生成器，控制器本身确定性运行，相同输入逐位复现。
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .buffer import BufferConfig, BufferSimulator
from .estimator import BandwidthEstimator, EstimatorConfig
from .metrics import SimulationMetrics
from .policy import ABRPolicy, PolicyConfig, PolicyContext
from .trace import Trace
from .types import (
    BitrateLevel,
    SegmentResult,
    SimulationError,
    ThroughputSample,
)


@dataclass(frozen=True, slots=True)
class SimulationConfig:
    """一次端到端模拟的顶层参数。"""

    segment_duration_s: float = 2.0
    start_level_index: int = 0
    estimator: EstimatorConfig = field(default_factory=EstimatorConfig)
    buffer: BufferConfig = field(default_factory=BufferConfig)
    policy: PolicyConfig = field(default_factory=PolicyConfig)

    def validate(self, n_levels: int) -> None:
        if self.segment_duration_s <= 0:
            raise SimulationError("segment_duration_s 必须为正数")
        if not (0 <= self.start_level_index < n_levels):
            raise SimulationError(
                f"start_level_index {self.start_level_index} 越界，共 {n_levels} 档"
            )


class SimulationController:
    """驱动单条轨迹上的完整 ABR 模拟。"""

    def __init__(self, trace: Trace, levels: tuple[BitrateLevel, ...],
                 config: SimulationConfig | None = None):
        self._trace = trace
        self._levels = levels
        self._cfg = config or SimulationConfig()
        self._cfg.validate(len(levels))
        self._estimator = BandwidthEstimator(self._cfg.estimator)
        buf_cfg = self._buffer_config()
        self._buffer = BufferSimulator(trace, buf_cfg)
        self._policy = ABRPolicy(self._cfg.policy)
        self._current = self._cfg.start_level_index
        self._consecutive_safe = 0
        self._cooldown = 0

    def _buffer_config(self) -> BufferConfig:
        # 统一分段时长，避免缓冲配置与顶层参数不一致
        b = self._cfg.buffer
        return BufferConfig(
            segment_duration_s=self._cfg.segment_duration_s,
            target_buffer_s=b.target_buffer_s,
            underflow_threshold_s=b.underflow_threshold_s,
            max_buffer_s=b.max_buffer_s,
            chunk_seconds=b.chunk_seconds,
            max_stall_seconds=b.max_stall_seconds,
            max_retries_per_chunk=b.max_retries_per_chunk,
        )

    # ------------------------------------------------------------------
    def run(self, n_segments: int) -> tuple[list[SegmentResult], SimulationMetrics]:
        if n_segments <= 0:
            raise SimulationError(f"n_segments 必须为正数，得到 {n_segments}")
        results: list[SegmentResult] = []
        metrics = SimulationMetrics()
        for seg_index in range(n_segments):
            seg = self._run_one(seg_index)
            results.append(seg)
            metrics.record(seg)
            if seg.failed:
                # 下载失败是不可恢复的终端状态：停止后续分段并给出诊断
                metrics.estimator_samples = self._estimator.estimate().sample_count
                raise SimulationError(
                    f"模拟在第 {seg_index} 段终止：{seg.failure_reason}"
                )
        metrics.estimator_samples = self._estimator.total_seen
        return results, metrics

    def _run_one(self, seg_index: int) -> SegmentResult:
        estimate = self._estimator.estimate()
        playback = self._buffer.playback_state()
        buffer_before = playback.buffer_seconds

        ctx = PolicyContext(
            segment_index=seg_index,
            levels=self._levels,
            current_index=self._current,
            estimate=estimate,
            playback=playback,
            consecutive_safe=self._consecutive_safe,
            cooldown_remaining=self._cooldown,
            segment_duration_s=self._cfg.segment_duration_s,
        )
        decision = self._policy.decide(ctx)
        chosen = self._levels[decision.chosen_index]

        throughput, download_s, underflow_s, failure = self._buffer.download_segment(
            seg_index, chosen
        )
        self._ingest(throughput, failure)
        self._update_state(decision)

        return SegmentResult(
            index=seg_index,
            chosen=chosen,
            decision=decision,
            throughput=throughput,
            download_seconds=download_s,
            buffer_before_s=buffer_before,
            buffer_after_s=self._buffer.buffer_s,
            underflow_seconds=underflow_s,
            in_protection=self._buffer.in_protection,
            failed=failure is not None,
            failure_reason=failure,
        )

    def _ingest(self, throughput: ThroughputSample, failure: str | None) -> None:
        # 失败段的吞吐不具参考价值（零带宽停滞），不污染估计器
        if failure is None:
            self._estimator.add_sample(throughput)

    def _update_state(self, decision) -> None:
        """根据决策更新当前档位、连续安全计数与冷却。"""
        switched = decision.chosen_index != self._current
        if switched:
            self._current = decision.chosen_index
            self._cooldown = self._cfg.policy.cooldown_segments
            # 任何切换都打断升档确认计数
            self._consecutive_safe = 0
        else:
            if self._cooldown > 0:
                self._cooldown -= 1
            # 确认计数只反映“带宽是否持续满足升档余量”这一独立信号，
            # 与确认/冷却/恢复门限是否满足无关，避免门限未达标时自我清零死锁
            if decision.upshift_safe:
                self._consecutive_safe += 1
            else:
                self._consecutive_safe = 0

    @property
    def buffer_simulator(self) -> BufferSimulator:
        return self._buffer
