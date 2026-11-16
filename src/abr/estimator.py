"""带宽估计器：滑动窗口 + 指数平滑 + 异常值抑制。

估计器消费分段下载产生的 :class:`~abr.types.ThroughputSample`，
向策略层提供统一的 :class:`~abr.types.BandwidthEstimate` 视图。
全部计算基于 NumPy 向量化实现，窗口长度有界，长轨迹内存可控。
"""

from __future__ import annotations

import math
from collections import deque
from dataclasses import dataclass

import numpy as np

from .types import BandwidthEstimate, SimulationError, ThroughputSample


@dataclass(frozen=True, slots=True)
class EstimatorConfig:
    """估计器参数。"""

    window_size: int = 8          # 滑动窗口容量（最近 N 个有效样本）
    smoothing_alpha: float = 0.35  # EMA 平滑系数，(0,1]，越大越跟手
    safety_discount: float = 0.85  # 安全折扣，估计带宽 = EMA * discount
    outlier_z: float = 2.5        # 异常值 z 分数阈值（窗口样本足够时生效）
    min_window_for_outlier: int = 4   # 窗口至少多少样本才启用 z 分数抑制
    max_reject_fraction: float = 0.25  # 异常拒绝占比上限，防止持续零有效样本

    def validate(self) -> None:
        if self.window_size < 2:
            raise SimulationError(f"window_size 至少为 2，得到 {self.window_size}")
        if not (0.0 < self.smoothing_alpha <= 1.0):
            raise SimulationError("smoothing_alpha 必须落在 (0,1]")
        if not (0.0 < self.safety_discount <= 1.0):
            raise SimulationError("safety_discount 必须落在 (0,1]")
        if self.outlier_z <= 0:
            raise SimulationError("outlier_z 必须为正数")


class BandwidthEstimator:
    """有状态带宽估计器；同一窗口状态可被策略反复读取。"""

    def __init__(self, config: EstimatorConfig | None = None):
        self._cfg = config or EstimatorConfig()
        self._cfg.validate()
        self._window: deque[float] = deque(maxlen=self._cfg.window_size)
        self._ema: float | None = None
        self._total_seen = 0
        self._rejected = 0

    # ------------------------------------------------------------------
    def add_sample(self, sample: ThroughputSample) -> bool:
        """摄入一个分段吞吐样本；返回是否被接受（False 表示被抑制/拒绝）。

        非有限值与负数是硬性非法输入，抛出 SimulationError；
        统计意义上的离群点被静默抑制并计入 rejected_outliers。
        """
        x = sample.throughput_kbps
        if not math.isfinite(x):
            raise SimulationError(
                f"第 {sample.segment_index} 段吞吐为非有限值 {x!r}，拒绝摄入估计器"
            )
        if x < 0.0:
            raise SimulationError(
                f"第 {sample.segment_index} 段吞吐为负数 {x!r}，拒绝摄入估计器"
            )
        self._total_seen += 1

        if self._is_outlier(x):
            self._rejected += 1
            # 拒绝比例超上限时强制接收，避免估计器在持续劣化网络中饿死
            seen = max(self._total_seen, 1)
            if self._rejected / seen <= self._cfg.max_reject_fraction:
                return False

        if self._ema is None:
            self._ema = x
        else:
            a = self._cfg.smoothing_alpha
            self._ema = a * x + (1.0 - a) * self._ema
        self._window.append(x)
        return True

    def _is_outlier(self, x: float) -> bool:
        if len(self._window) < self._cfg.min_window_for_outlier:
            return False
        arr = np.fromiter(self._window, dtype=np.float64)
        mean = float(np.mean(arr))
        std = float(np.std(arr))
        if std <= 1e-9:
            # 窗口近乎常数：以 2 倍 / 腰斩作为离群判据（真实阶跃会被
            # max_reject_fraction 兜底强制接收，估计器不会饿死）
            return x > mean * 2.0 or (mean > 0 and x < mean * 0.5)
        z = abs(x - mean) / std
        return z > self._cfg.outlier_z

    # ------------------------------------------------------------------
    def estimate(self) -> BandwidthEstimate:
        """输出当前统一估计；尚无样本时给出零带宽保守估计。"""
        if not self._window or self._ema is None:
            return BandwidthEstimate(
                estimated_kbps=0.0,
                raw_ema_kbps=0.0,
                window_mean_kbps=0.0,
                trend_kbps_per_sample=0.0,
                sample_count=0,
                rejected_outliers=self._rejected,
            )
        arr = np.fromiter(self._window, dtype=np.float64)
        mean = float(np.mean(arr))
        trend = self._slope(arr)
        return BandwidthEstimate(
            estimated_kbps=self._ema * self._cfg.safety_discount,
            raw_ema_kbps=self._ema,
            window_mean_kbps=mean,
            trend_kbps_per_sample=trend,
            sample_count=len(self._window),
            rejected_outliers=self._rejected,
        )

    @staticmethod
    def _slope(arr: np.ndarray) -> float:
        """对窗口样本做最小二乘斜率（单位：kbit/s 每样本）。"""
        n = len(arr)
        if n < 2:
            return 0.0
        x = np.arange(n, dtype=np.float64)
        xm, ym = x.mean(), arr.mean()
        denom = float(np.sum((x - xm) ** 2))
        if denom <= 0.0:
            return 0.0
        return float(np.sum((x - xm) * (arr - ym)) / denom)

    @property
    def total_seen(self) -> int:
        return self._total_seen
