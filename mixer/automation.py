"""分段线性自动化曲线。

规则:
- 构造时拒绝 NaN/无穷大取值与负时间、重复时间点;
- 输入点允许乱序,内部按时间排序;
- 采样区间外保持首/尾值(不外推);
- 空曲线等价于恒为 1.0(即只保留静态增益)。

曲线以轨道本地采样点为时间单位,由混音引擎在块边界逐点采样。
"""

from __future__ import annotations

import math

import numpy as np

from .types import AutomationPoint, ConfigError, TrackSpec


class AutomationCurve:
    """不可变的分段线性曲线,支持标量与数组采样。"""

    def __init__(self, points: tuple[AutomationPoint, ...] | list[AutomationPoint]) -> None:
        pts = list(points)
        for p in pts:
            if not isinstance(p.time, int) or p.time < 0:
                raise ConfigError("自动化时间必须是非负整数采样点")
            if math.isnan(p.value) or math.isinf(p.value):
                raise ConfigError("自动化取值不允许 NaN 或无穷大")
        times = [p.time for p in pts]
        if len(set(times)) != len(times):
            raise ConfigError("自动化曲线存在重复时间点")
        pts.sort(key=lambda p: p.time)
        self._points = tuple(pts)
        self._times = np.array([p.time for p in pts], dtype=np.int64)
        self._values = np.array([p.value for p in pts], dtype=np.float64)

    @property
    def points(self) -> tuple[AutomationPoint, ...]:
        return self._points

    def __len__(self) -> int:
        return len(self._points)

    def sample(self, positions: np.ndarray) -> np.ndarray:
        """对整数采样点数组向量化采样,区间外保持首尾值。"""

        pos = np.asarray(positions, dtype=np.int64)
        if np.any(pos < 0):
            raise ConfigError("自动化采样位置不能为负")
        if len(self._points) == 0:
            return np.ones(pos.shape, dtype=np.float64)
        if len(self._points) == 1:
            return np.full(pos.shape, self._values[0], dtype=np.float64)

        # searchsorted: 右侧区间索引,clip 到 [1, n-1] 后由首尾保持处理越界
        idx = np.searchsorted(self._times, pos, side="right")
        idx_clip = np.clip(idx, 1, len(self._points) - 1)
        t0 = self._times[idx_clip - 1]
        t1 = self._times[idx_clip]
        v0 = self._values[idx_clip - 1]
        v1 = self._values[idx_clip]
        span = t1 - t0
        frac = np.where(span > 0, (pos - t0) / np.where(span > 0, span, 1), 0.0)
        result = v0 + (v1 - v0) * frac

        # 区间外保持:pos <= 首个时间取首值,>= 末个时间取尾值
        result = np.where(pos <= self._times[0], self._values[0], result)
        result = np.where(pos >= self._times[-1], self._values[-1], result)
        return result

    def sample_scalar(self, position: int) -> float:
        """单点采样的便捷封装。"""

        return float(self.sample(np.array([position], dtype=np.int64))[0])


def curve_for_track(track: TrackSpec) -> AutomationCurve:
    """从轨道声明构建曲线;无自动化点时得到恒 1.0 曲线。"""

    return AutomationCurve(track.automation)


def total_automation_samples(tracks: tuple[TrackSpec, ...] | list[TrackSpec]) -> int:
    """统计工程内全部自动化控制点数量(报告字段 automation_samples)。"""

    return sum(len(t.automation) for t in tracks)
