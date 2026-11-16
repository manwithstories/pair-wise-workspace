"""可复现的网络轨迹生成器。

所有随机行为均由 ``numpy.random.Generator(seed)`` 驱动，相同种子 +
相同配置逐位复现。输出 :class:`~abr.types.NetworkSample` 序列，并通过
:class:`Trace` 提供按时间插值的查询接口，供缓冲模拟器在分段下载中
读取瞬时带宽/延迟/丢包。
"""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .types import NetworkSample, TraceError, TracePattern


@dataclass(frozen=True, slots=True)
class TraceConfig:
    """轨迹生成参数。"""

    duration_s: float
    timestep_s: float = 0.1
    pattern: TracePattern = TracePattern.MIXED
    seed: int = 42
    base_bandwidth_kbps: float = 3000.0
    min_bandwidth_kbps: float = 150.0
    base_latency_ms: float = 40.0
    base_loss_rate: float = 0.0
    jitter_strength: float = 0.12   # 乘性噪声标准差（占当前带宽比例）

    def validate(self) -> None:
        if not isinstance(self.duration_s, (int, float)) or self.duration_s <= 0:
            raise TraceError(f"duration_s 必须为正数，得到 {self.duration_s!r}")
        if self.timestep_s <= 0:
            raise TraceError(f"timestep_s 必须为正数，得到 {self.timestep_s!r}")
        if not math.isfinite(self.base_bandwidth_kbps) or self.base_bandwidth_kbps <= 0:
            raise TraceError("base_bandwidth_kbps 必须为正有限值")
        if not (0.0 <= self.base_loss_rate < 1.0):
            raise TraceError(f"base_loss_rate 必须落在 [0,1)，得到 {self.base_loss_rate}")
        if not isinstance(self.pattern, TracePattern):
            raise TraceError(f"未知轨迹模式: {self.pattern!r}")


class Trace:
    """网络采样序列，支持按时间插值查询。"""

    def __init__(self, samples: list[NetworkSample], pattern: TracePattern, seed: int):
        if not samples:
            raise TraceError("轨迹为空：至少需要一个采样点")
        self._samples = samples
        self._times = np.array([s.time_s for s in samples], dtype=np.float64)
        self._bw = np.array([s.bandwidth_kbps for s in samples], dtype=np.float64)
        self._lat = np.array([s.latency_ms for s in samples], dtype=np.float64)
        self._loss = np.array([s.loss_rate for s in samples], dtype=np.float64)
        if not np.all(np.isfinite(self._bw)) or not np.all(np.isfinite(self._lat)):
            raise TraceError("轨迹包含非有限值（NaN/Inf）")
        if np.any(self._bw < 0.0) or np.any(self._lat < 0.0):
            raise TraceError("轨迹包含负带宽或负延迟")
        if np.any((self._loss < 0.0) | (self._loss >= 1.0)):
            raise TraceError("丢包率必须落在 [0,1)")
        if float(np.max(self._bw)) <= 0.0:
            raise TraceError("整条轨迹带宽恒为零，无法进行任何分段下载")
        self.pattern = pattern
        self.seed = seed

    @property
    def samples(self) -> list[NetworkSample]:
        return self._samples

    @property
    def duration_s(self) -> float:
        return float(self._times[-1])

    def sample_at(self, time_s: float) -> NetworkSample:
        """返回 ``time_s`` 时刻的网络状态（带宽/延迟线性插值，丢包取右段常数）。

        时间超出轨迹范围时抛出 TraceError，调用方可据此诊断“轨迹耗尽”。
        """
        if not math.isfinite(time_s) or time_s < 0.0:
            raise TraceError(f"非法查询时间: {time_s!r}")
        if time_s > self._times[-1]:
            raise TraceError(
                f"轨迹耗尽：查询时刻 {time_s:.3f}s 超过轨迹终点 {self._times[-1]:.3f}s"
            )
        idx = int(np.searchsorted(self._times, time_s, side="right")) - 1
        idx = max(0, min(idx, len(self._times) - 2))
        t0, t1 = self._times[idx], self._times[idx + 1]
        frac = 0.0 if t1 == t0 else (time_s - t0) / (t1 - t0)
        return NetworkSample(
            time_s=time_s,
            bandwidth_kbps=float(self._bw[idx] * (1.0 - frac) + self._bw[idx + 1] * frac),
            latency_ms=float(self._lat[idx] * (1.0 - frac) + self._lat[idx + 1] * frac),
            loss_rate=float(self._loss[min(idx + 1, len(self._loss) - 1)]),
        )


class TraceGenerator:
    """按 :class:`TraceConfig` 生成确定性轨迹。"""

    # 非突发场景下带宽的最小正下限，零带宽只允许出现在显式突发窗口内
    _FLOOR_KBPS = 50.0

    def __init__(self, config: TraceConfig):
        config.validate()
        self._cfg = config
        self._rng = np.random.default_rng(config.seed)

    def generate(self) -> Trace:
        cfg = self._cfg
        n = max(1, int(round(cfg.duration_s / cfg.timestep_s)) + 1)
        t = np.arange(n, dtype=np.float64) * cfg.timestep_s
        bw, lat, loss = self._dispatch(t)
        # 记录显式零带宽窗口（突发丢失），加噪/钳底后恢复为零
        zero_mask = bw <= 0.0
        # 统一乘性抖动（突发窗口之外）
        noise = self._rng.lognormal(mean=0.0, sigma=cfg.jitter_strength, size=n)
        bw = bw * noise
        bw = np.maximum(bw, self._FLOOR_KBPS)
        bw[zero_mask] = 0.0
        samples = [
            NetworkSample(
                time_s=float(ti),
                bandwidth_kbps=float(b),
                latency_ms=float(max(0.0, l)),
                loss_rate=float(min(max(p, 0.0), 0.999)),
            )
            for ti, b, l, p in zip(t, bw, lat, loss)
        ]
        return Trace(samples, cfg.pattern, cfg.seed)

    # ------------------------------------------------------------------
    def _dispatch(self, t: np.ndarray):
        p = self._cfg.pattern
        if p is TracePattern.STABLE:
            return self._stable(t)
        if p is TracePattern.STEP_DOWN:
            return self._step_down(t)
        if p is TracePattern.STEP_RECOVERY:
            return self._step_recovery(t)
        if p is TracePattern.PERIODIC_JITTER:
            return self._periodic(t)
        if p is TracePattern.BURST_LOSS:
            return self._burst(t)
        return self._mixed(t)

    def _base_arrays(self, n: int):
        cfg = self._cfg
        bw = np.full(n, cfg.base_bandwidth_kbps, dtype=np.float64)
        lat = np.full(n, cfg.base_latency_ms, dtype=np.float64)
        loss = np.full(n, cfg.base_loss_rate, dtype=np.float64)
        return bw, lat, loss

    def _stable(self, t):
        return self._base_arrays(len(t))

    def _step_down(self, t):
        bw, lat, loss = self._base_arrays(len(t))
        drop = t >= self._cfg.duration_s * 0.4
        bw[drop] = self._cfg.min_bandwidth_kbps
        lat[drop] += 25.0
        loss[drop] = np.maximum(loss[drop], 0.03)
        return bw, lat, loss

    def _step_recovery(self, t):
        bw, lat, loss = self._base_arrays(len(t))
        d = self._cfg.duration_s
        low = (t >= d * 0.3) & (t < d * 0.6)
        bw[low] = self._cfg.min_bandwidth_kbps
        lat[low] += 30.0
        loss[low] = np.maximum(loss[low], 0.04)
        return bw, lat, loss

    def _periodic(self, t):
        bw, lat, loss = self._base_arrays(len(t))
        osc = 0.5 + 0.5 * np.sin(2.0 * np.pi * t / max(self._cfg.duration_s / 4.0, 1e-6))
        lo = self._cfg.min_bandwidth_kbps
        hi = self._cfg.base_bandwidth_kbps
        bw = lo + (hi - lo) * osc
        return bw, lat, loss

    def _burst(self, t):
        bw, lat, loss = self._base_arrays(len(t))
        d = self._cfg.duration_s
        # 两个固定相对位置的突发窗口（窗口位置由时长决定，时长内形状由种子决定）
        for center, width in ((d * 0.35, max(d * 0.04, 0.4)),
                              (d * 0.72, max(d * 0.03, 0.3))):
            window = np.abs(t - center) <= width / 2.0
            bw[window] = 0.0
            lat[window] += 120.0
            loss[window] = 0.35
        return bw, lat, loss

    def _mixed(self, t):
        """组合场景：平稳 → 阶跃下降 → 周期恢复 → 突发丢失。"""
        cfg = self._cfg
        d = cfg.duration_s
        bw, lat, loss = self._base_arrays(len(t))
        # 阶段 2：30%-55% 处阶跃下降
        low = (t >= d * 0.30) & (t < d * 0.55)
        bw[low] = cfg.min_bandwidth_kbps
        lat[low] += 30.0
        loss[low] = np.maximum(loss[low], 0.04)
        # 阶段 3：55%-80% 处正弦恢复振荡
        rec = (t >= d * 0.55) & (t < d * 0.80)
        phase = (t[rec] - d * 0.55) / max(d * 0.25, 1e-6)
        osc = 0.5 + 0.5 * np.sin(2.0 * np.pi * phase)
        bw[rec] = cfg.min_bandwidth_kbps + (cfg.base_bandwidth_kbps - cfg.min_bandwidth_kbps) * osc
        # 阶段 4：85% 附近一次短时突发零带宽
        burst = np.abs(t - d * 0.85) <= max(d * 0.02, 0.3) / 2.0
        bw[burst] = 0.0
        lat[burst] += 120.0
        loss[burst] = 0.35
        return bw, lat, loss
