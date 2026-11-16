"""码率决策引擎的核心不可变数据结构与异常类型。

所有跨模块流转的数据都使用 ``@dataclass(frozen=True, slots=True)``，
保证模拟过程中状态不会被意外篡改，也便于序列化与确定性复现。
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum


# ---------------------------------------------------------------------------
# 异常体系：所有可诊断错误都继承 ABRError，CLI 据此映射为退出码 2
# ---------------------------------------------------------------------------
class ABRError(Exception):
    """ABR 引擎所有可诊断错误的基类。"""


class ConfigError(ABRError):
    """码率档位 / 模拟参数非法（严格递增、分段时长、有限值等）。"""


class TraceError(ABRError):
    """网络轨迹非法（全零带宽、非有限值、样本耗尽等）。"""


class SimulationError(ABRError):
    """模拟过程中出现不可恢复状态（持续零带宽、NaN 样本等）。"""


# ---------------------------------------------------------------------------
# 枚举
# ---------------------------------------------------------------------------
class TracePattern(str, Enum):
    """轨迹生成器支持的网络模式。"""

    STABLE = "stable"            # 稳定带宽 + 小幅噪声
    STEP_DOWN = "step_down"      # 中途阶跃下降
    STEP_RECOVERY = "step_recovery"  # 阶跃下降后恢复
    PERIODIC_JITTER = "periodic_jitter"  # 周期性抖动（正弦 + 噪声）
    BURST_LOSS = "burst_loss"    # 突发丢包 / 短时零带宽
    MIXED = "mixed"              # 组合场景（演示默认）


class DecisionKind(str, Enum):
    """决策相对当前档位的方向。"""

    HOLD = "hold"
    UPSHIFT = "upshift"
    DOWNSHIFT = "downshift"


class SwitchReason(str, Enum):
    """档位切换的归因，用于区分主动切换与被迫降档。"""

    NONE = "none"                          # 未切换
    OPPORTUNISTIC = "opportunistic"        # 主动升档（带宽安全余量充足）
    FEASIBILITY = "feasibility"            # 被迫降档（当前档预计无法按时下载完）
    UNDERFLOW_PROTECTION = "underflow_protection"  # 下溢保护触发的最低档
    RECOVERY_HYSTERESIS = "recovery_hysteresis"    # 恢复滞后窗口内保持低档


# ---------------------------------------------------------------------------
# 静态描述类
# ---------------------------------------------------------------------------
@dataclass(frozen=True, slots=True)
class BitrateLevel:
    """单个码率档位。

    bitrate_kbps 为该档视频码率（千比特/秒），必须严格递增。
    """

    index: int
    bitrate_kbps: float

    def segment_bytes(self, segment_seconds: float) -> int:
        """该档位一个分段的字节数（kbit/s * s / 8 * 1000）。"""
        return int(round(self.bitrate_kbps * segment_seconds * 1000.0 / 8.0))


@dataclass(frozen=True, slots=True)
class NetworkSample:
    """一个时间点的网络采样（轨迹生成器的输出单元）。"""

    time_s: float          # 采样时间戳（秒）
    bandwidth_kbps: float  # 瞬时可用带宽（千比特/秒）
    latency_ms: float      # 单向传播延迟（毫秒）
    loss_rate: float       # 丢包率，[0, 1)


@dataclass(frozen=True, slots=True)
class ThroughputSample:
    """一次完整分段下载得到的吞吐观测（估计器输入单元）。"""

    segment_index: int
    throughput_kbps: float  # 实际有效下载吞吐（含重试代价），千比特/秒
    duration_s: float       # 该段下载总耗时（含延迟与重试）
    bytes_sent: int         # 实际传输的总字节（含重传）
    loss_rate: float        # 该段经历的平均丢包率


# ---------------------------------------------------------------------------
# 估计与播放状态
# ---------------------------------------------------------------------------
@dataclass(frozen=True, slots=True)
class BandwidthEstimate:
    """估计器对当前可用带宽的统一输出。"""

    estimated_kbps: float   # 折扣后的安全估计带宽
    raw_ema_kbps: float     # 未折扣的 EMA 平滑值
    window_mean_kbps: float  # 滑动窗口算术均值
    trend_kbps_per_sample: float  # 每样本的带宽趋势（正=上升，负=下降）
    sample_count: int       # 累计消费的有效样本数
    rejected_outliers: int  # 累计被异常值抑制拒绝的样本数


@dataclass(frozen=True, slots=True)
class PlaybackState:
    """某分段决策时刻的播放缓冲快照。"""

    buffer_seconds: float       # 当前缓冲水位（秒）
    target_buffer_s: float      # 目标缓冲（秒）
    underflow_threshold_s: float  # 下溢保护阈值（秒）
    in_protection: bool         # 是否处于下溢保护状态
    playback_time_s: float      # 已播放时长（秒）
    total_underflow_s: float    # 累计真实下溢时长（秒）
    protection_time_s: float    # 累计保护状态时长（秒）


# ---------------------------------------------------------------------------
# 决策与分段结果
# ---------------------------------------------------------------------------
@dataclass(frozen=True, slots=True)
class DecisionResult:
    """策略对单个分段的选档结果。"""

    segment_index: int
    chosen_index: int
    previous_index: int
    kind: DecisionKind
    reason: SwitchReason
    predicted_download_s: float  # 预计下载耗时（秒）
    safety_margin: float         # 估计带宽相对所选档码率的倍数余量
    risk_rejected: bool = False  # 是否因风险（预计下溢）拒绝了更高档位
    cooldown_applied: bool = False  # 是否因冷却周期抑制了切换
    upshift_safe: bool = False   # 当前带宽是否满足升档安全余量（确认计数信号）


@dataclass(frozen=True, slots=True)
class SegmentResult:
    """一个分段从决策到下载完成的完整记录（指标模块消费）。"""

    index: int
    chosen: BitrateLevel
    decision: DecisionResult
    throughput: ThroughputSample
    download_seconds: float
    buffer_before_s: float
    buffer_after_s: float
    underflow_seconds: float     # 该段经历的真实下溢时长
    in_protection: bool
    failed: bool = False         # 下载是否失败（零带宽耗尽等）
    failure_reason: str | None = None


def validate_bitrates(bitrates_kbps: list[float]) -> list[BitrateLevel]:
    """校验并构造严格递增的码率档位表。

    抛出 ConfigError 的情形：档位少于 2、含非有限值/负数、未严格递增。
    """

    if len(bitrates_kbps) < 2:
        raise ConfigError("至少需要 2 个码率档位")
    levels: list[BitrateLevel] = []
    prev: float | None = None
    for i, value in enumerate(bitrates_kbps):
        if not isinstance(value, (int, float)) or isinstance(value, bool):
            raise ConfigError(f"第 {i} 个码率不是数值: {value!r}")
        fv = float(value)
        if not _is_finite(fv) or fv <= 0:
            raise ConfigError(f"第 {i} 个码率必须为正有限值，得到 {fv!r}")
        if prev is not None and fv <= prev:
            raise ConfigError(
                f"码率档位必须严格递增：位置 {i} 的 {fv} 未大于前值 {prev}"
            )
        levels.append(BitrateLevel(index=i, bitrate_kbps=fv))
        prev = fv
    return levels


def _is_finite(x: float) -> bool:
    """局部 finite 判断，避免在热路径上重复 import math。"""
    import math

    return math.isfinite(x)
