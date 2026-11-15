"""关键帧自动化曲线与控制速率采样。

关键帧以秒定位、值取 dB。求值规则:
* 首帧时间之前保持首值，末帧时间之后保持末值；
* 关键帧之间按时间线性插值 dB；
* 同一时间的重复关键帧在工程解析阶段已按“后者为准”去重；
* NaN/Inf 在解析阶段拒绝。

每 hop 个目标采样点求一次曲线 dB，控制值之间再线性插值成逐样本增益。
所有插值都在整数采样下标空间完成（关键帧时间预先换算成采样下标），
不经过“秒”的浮点往返，因此增益只依赖全局采样下标，按任意块长切分
都能逐样本复现，块边界不保存任何相位状态。
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .project import Keyframe, Track


def _stable_interp(x: np.ndarray, xp: np.ndarray, fp: np.ndarray,
                   left: float, right: float) -> np.ndarray:
    """块无关的线性插值：同一查询点无论怎么分块都得到同一位模式。"""
    x = np.asarray(x, dtype=np.float64)
    if xp.size == 1:
        return np.full_like(x, fp[0], dtype=np.float64)
    idx = np.searchsorted(xp, x, side="right") - 1
    below = x <= xp[0]
    above = x >= xp[-1]
    idx = np.clip(idx, 0, len(xp) - 2)
    x0 = xp[idx]
    x1 = xp[idx + 1]
    with np.errstate(divide="ignore", invalid="ignore"):
        alpha = (x - x0) / (x1 - x0)
        result = fp[idx] + alpha * (fp[idx + 1] - fp[idx])
    result = np.where(below, left, result)
    result = np.where(above, right, result)
    return result


class Automation:
    """一条自动化曲线的求值器（输出线性增益，全程 float64）。"""

    def __init__(self, frames: list[Keyframe], sample_rate: int, hop: int):
        if not frames:
            raise ValueError("自动化曲线不能为空")
        if hop <= 0:
            raise ValueError(f"hop 必须为正整数，得到 {hop}")
        if sample_rate <= 0:
            raise ValueError(f"采样率必须为正数，得到 {sample_rate}")
        self.frames = sorted(frames, key=lambda item: item.time)
        self.sample_rate = int(sample_rate)
        self.hop = int(hop)
        # 关键帧时间换算成浮点采样下标，只做一次。
        self.frame_indexes = np.array(
            [frame.time * sample_rate for frame in self.frames], dtype=np.float64
        )
        self.dbs = np.array([frame.db for frame in self.frames], dtype=np.float64)

    def db_at_samples(self, sample_indexes: np.ndarray) -> np.ndarray:
        """按整数采样下标求值；首帧前钳到首值、末帧后保持末值。

        使用显式区间插值而非 np.interp：np.interp 在传入不同控制点
        子集时浮点路径不同，会破坏任意块长下的逐样本一致性。
        """
        indexes = np.asarray(sample_indexes, dtype=np.float64)
        return _stable_interp(
            indexes, self.frame_indexes, self.dbs,
            left=self.dbs[0], right=self.dbs[-1],
        ).astype(np.float64)

    def db_at_seconds(self, times: np.ndarray) -> np.ndarray:
        times = np.asarray(times, dtype=np.float64)
        return self.db_at_samples(times * self.sample_rate)

    def gains(self, start_sample: int, count: int) -> np.ndarray:
        """返回 [start_sample, start_sample + count) 的逐样本线性增益。

        控制点始终取覆盖查询区间的两个相邻 hop 边界（必要时向外扩一个
        hop），保证任何块长下每个查询样本都落在同一条全局控制点线段上，
        从而与切分方式逐样本一致。
        """
        if count <= 0:
            return np.empty(0, dtype=np.float64)
        end_sample = start_sample + count
        first_control = (start_sample // self.hop) * self.hop
        last_control = ((end_sample - 1) // self.hop) * self.hop
        # 向外多取一个 hop，确保首尾查询样本都有左右两个控制点。
        control_indexes = np.arange(
            first_control, last_control + 2 * self.hop, self.hop, dtype=np.int64
        )
        control_db = self.db_at_samples(control_indexes)
        sample_indexes = np.arange(start_sample, end_sample, dtype=np.int64)
        db_values = _stable_interp(
            sample_indexes.astype(np.float64),
            control_indexes.astype(np.float64),
            control_db,
            left=control_db[0], right=control_db[-1],
        )
        return np.power(10.0, db_values / 20.0).astype(np.float64)

    def control_report(self, total_samples: int) -> dict:
        point_count = int(np.ceil(total_samples / self.hop))
        control_indexes = np.arange(point_count, dtype=np.int64) * self.hop
        control_db = self.db_at_samples(control_indexes)
        return {
            "hop": self.hop,
            "samples": point_count,
            "first_time": float(control_indexes[0] / self.sample_rate),
            "last_time": float(control_indexes[-1] / self.sample_rate),
            "first_db": float(control_db[0]),
            "last_db": float(control_db[-1]),
        }


def build_track_automation(track: Track, sample_rate: int, hop: int) -> Automation | None:
    if not track.automation:
        return None
    return Automation(track.automation, sample_rate=sample_rate, hop=hop)
