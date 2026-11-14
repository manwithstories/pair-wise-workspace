"""吞吐估计器：统一接口，三种可互换实现。

接口约定：
    add_sample(throughput_kbps)  每下载完一个分片喂入一次实测吞吐
    estimate()                   返回当前带宽估计（kbps），无样本时返回 0

决策器只依赖该接口注入的实例，不读任何全局状态。
"""

from __future__ import annotations

from collections import deque
from typing import Protocol

EPS = 1e-9


class Estimator(Protocol):
    def add_sample(self, throughput_kbps: float) -> None:
        ...

    def estimate(self) -> float:
        ...


class HarmonicMeanEstimator:
    """滑动窗口调和平均：对低样本敏感，偏保守，抗瞬时虚高。"""

    def __init__(self, window: int = 8) -> None:
        self._samples: deque[float] = deque(maxlen=window)

    def add_sample(self, throughput_kbps: float) -> None:
        if throughput_kbps > 0:
            self._samples.append(float(throughput_kbps))

    def estimate(self) -> float:
        n = len(self._samples)
        if n == 0:
            return 0.0
        denom = sum(1.0 / max(s, EPS) for s in self._samples)
        return n / denom if denom > 0 else 0.0


class EwmaEstimator:
    """快慢双 EWMA：快通道跟趋势、慢通道抑抖动，取较小值防过估计。"""

    def __init__(self, fast_alpha: float = 0.3, slow_alpha: float = 0.1) -> None:
        self._fast_alpha = fast_alpha
        self._slow_alpha = slow_alpha
        self._fast: float | None = None
        self._slow: float | None = None

    def add_sample(self, throughput_kbps: float) -> None:
        x = float(throughput_kbps)
        if x <= 0:
            return
        self._fast = x if self._fast is None else self._fast_alpha * x + (1.0 - self._fast_alpha) * self._fast
        self._slow = x if self._slow is None else self._slow_alpha * x + (1.0 - self._slow_alpha) * self._slow

    def estimate(self) -> float:
        if self._fast is None or self._slow is None:
            return 0.0
        return min(self._fast, self._slow)


class DualModeEstimator:
    """双模鲁棒估计：平稳模式取窗口截尾均值与慢 EWMA 的较小值；
    骤降模式用最近 3 个样本的调和平均（乘 1.2 容差）快速下压。

    两模式取小：平稳期贴近趋势、抗抖动；带宽骤降时 1~2 个分片内
    跟上真实水平，避免高档位长下载抽干缓冲。
    """

    def __init__(self, window: int = 12, trim_ratio: float = 0.2,
                 slow_alpha: float = 0.15, fast_window: int = 3,
                 fast_tolerance: float = 1.2) -> None:
        self._samples: deque[float] = deque(maxlen=window)
        self._trim = trim_ratio
        self._slow_alpha = slow_alpha
        self._fast_window = fast_window
        self._fast_tolerance = fast_tolerance
        self._slow: float | None = None

    def add_sample(self, throughput_kbps: float) -> None:
        x = float(throughput_kbps)
        if x <= 0:
            return
        self._samples.append(x)
        self._slow = x if self._slow is None else self._slow_alpha * x + (1.0 - self._slow_alpha) * self._slow

    def estimate(self) -> float:
        if not self._samples or self._slow is None:
            return 0.0
        ordered = sorted(self._samples)
        k = int(len(ordered) * self._trim)
        core = ordered[k:len(ordered) - k] if len(ordered) - 2 * k > 0 else ordered
        trimmed_mean = sum(core) / len(core)
        recent = list(self._samples)[-self._fast_window:]
        fast = len(recent) / sum(1.0 / max(s, EPS) for s in recent) * self._fast_tolerance
        return min(trimmed_mean, self._slow, fast)


ESTIMATOR_REGISTRY: dict[str, type] = {
    "harmonic": HarmonicMeanEstimator,
    "ewma": EwmaEstimator,
    "dual": DualModeEstimator,
}


def make_estimator(name: str) -> Estimator:
    """按名构造估计器；未知名称抛 KeyError，由 CLI 映射为退出码 2。"""
    return ESTIMATOR_REGISTRY[name]()
