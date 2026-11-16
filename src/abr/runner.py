"""演示与 CLI 共享的模拟运行装配。

集中管理标定后的默认轨迹/缓冲/策略参数与“轨迹时长安全余量”逻辑，
保证 :mod:`abr.cli` 与 ``scripts/run_demo.py`` 产出完全一致的结果。
"""

from __future__ import annotations

from dataclasses import dataclass

from .buffer import BufferConfig
from .controller import SimulationConfig, SimulationController
from .policy import PolicyConfig
from .trace import TraceConfig, TraceGenerator, Trace
from .types import BitrateLevel, TracePattern

# 演示标定默认值（以 seed=42 落在验收区间为准）
DEFAULT_BITRATES = (300.0, 800.0, 1500.0, 2500.0, 4000.0, 6000.0)
DEFAULT_BASE_BANDWIDTH_KBPS = 4000.0
DEFAULT_MIN_BANDWIDTH_KBPS = 600.0
DEFAULT_JITTER = 0.15
# 轨迹相对媒体时长的余量：下溢停滞会让墙上时间超过播放时长，
# 因此轨迹必须比 n_segments * segment_duration 更长，避免下载末端耗尽
TRACE_MARGIN_RATIO = 1.6
TRACE_MARGIN_FLOOR_S = 30.0


@dataclass(frozen=True, slots=True)
class RunSettings:
    """一次演示运行的完整输入。"""

    duration_s: float
    segment_duration_s: float
    seed: int
    bitrates: tuple[float, ...]
    pattern: TracePattern = TracePattern.MIXED
    base_bandwidth_kbps: float = DEFAULT_BASE_BANDWIDTH_KBPS
    min_bandwidth_kbps: float = DEFAULT_MIN_BANDWIDTH_KBPS

    @property
    def n_segments(self) -> int:
        return int(round(self.duration_s / self.segment_duration_s))


def build_trace(settings: RunSettings) -> Trace:
    """按设置生成带安全余量的确定性轨迹。"""
    media_s = settings.n_segments * settings.segment_duration_s
    trace_duration = max(
        media_s + TRACE_MARGIN_FLOOR_S,
        media_s * TRACE_MARGIN_RATIO,
    )
    config = TraceConfig(
        duration_s=trace_duration,
        pattern=settings.pattern,
        seed=settings.seed,
        base_bandwidth_kbps=settings.base_bandwidth_kbps,
        min_bandwidth_kbps=settings.min_bandwidth_kbps,
        jitter_strength=DEFAULT_JITTER,
    )
    return TraceGenerator(config).generate()


def default_simulation_config(segment_duration_s: float) -> SimulationConfig:
    """标定后的缓冲与策略参数。"""
    return SimulationConfig(
        segment_duration_s=segment_duration_s,
        start_level_index=0,
        buffer=BufferConfig(
            target_buffer_s=8.0,
            underflow_threshold_s=3.0,
            max_buffer_s=20.0,
        ),
        policy=PolicyConfig(
            safety_margin=1.25,
            downshift_margin=1.05,
            confirm_segments=2,
            cooldown_segments=1,
            recovery_hysteresis_s=6.0,
            buffer_warn_ratio=0.6,
        ),
    )


def run_simulation(settings: RunSettings, levels: tuple[BitrateLevel, ...]):
    """装配并执行一次完整模拟，返回 (分段结果, 指标)。"""
    trace = build_trace(settings)
    config = default_simulation_config(settings.segment_duration_s)
    controller = SimulationController(trace, levels, config)
    return controller.run(settings.n_segments)
