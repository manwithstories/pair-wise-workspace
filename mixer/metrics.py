"""混音统计:峰值、RMS、有效值与削波计数。

所有聚合以 float64 流式累加,块大小不改变最终求和结果
(加法结合顺序在相同分块序列下一致;离线/流式仅块大小不同,
逐采样写出的 WAV 仍保证一致,统计差异远低于 1e-6 容差)。

分贝约定:dBFS = 20*log10(amplitude);静音以 floor_dbfs 兜底,
避免向 JSON 写出 -inf。
"""

from __future__ import annotations

import math

import numpy as np


def to_dbfs(amplitude: float, floor_dbfs: float = -120.0) -> float:
    """线性幅度转 dBFS,静音落到 floor。"""

    if amplitude <= 0.0 or not math.isfinite(amplitude):
        return float(floor_dbfs)
    return float(20.0 * math.log10(amplitude))


def count_clipped(signal: np.ndarray, threshold: float) -> int:
    """任一声道越界即计一个削波采样。"""

    arr = np.asarray(signal, dtype=np.float64)
    if arr.size == 0:
        return 0
    if arr.ndim == 1:
        return int(np.count_nonzero(np.abs(arr) >= threshold))
    return int(np.count_nonzero(np.any(np.abs(arr) >= threshold, axis=1)))


class MetricsAccumulator:
    """跨块累积主总线统计。"""

    def __init__(self, clip_threshold: float = 1.0, floor_dbfs: float = -120.0) -> None:
        self.clip_threshold = float(clip_threshold)
        self.floor_dbfs = float(floor_dbfs)
        self._sum_sq = 0.0
        self._peak = 0.0
        self._clipped = 0
        self._frames = 0
        self._active_frames = 0

    def add(self, block: np.ndarray) -> None:
        """纳入一个 (n, 2) 或 1D 块。"""

        arr = np.asarray(block, dtype=np.float64)
        if arr.size == 0:
            return
        if arr.ndim == 1:
            arr = arr[:, None]
        self._frames += arr.shape[0]
        self._sum_sq += float(np.sum(arr * arr, dtype=np.float64))
        block_peak = float(np.max(np.abs(arr))) if arr.size else 0.0
        if block_peak > self._peak:
            self._peak = block_peak
        self._clipped += count_clipped(arr, self.clip_threshold)
        # 有效帧:任一声道非零
        self._active_frames += int(np.count_nonzero(np.any(arr != 0.0, axis=1)))

    @property
    def frames(self) -> int:
        return self._frames

    @property
    def peak(self) -> float:
        return self._peak

    @property
    def clipped_samples(self) -> int:
        return self._clipped

    def rms(self) -> float:
        """全帧 RMS(静音帧计入分母)。"""

        if self._frames == 0:
            return 0.0
        # 双声道:每帧每声道平方和 / 2 后再对帧求平均
        per_frame = self._sum_sq / 2.0 / self._frames
        return math.sqrt(max(0.0, per_frame))

    def effective_rms(self) -> float:
        """有效值 RMS:仅对非静音帧求平均,反映实际有声段落响度。"""

        if self._active_frames == 0:
            return 0.0
        per_frame = self._sum_sq / 2.0 / self._active_frames
        return math.sqrt(max(0.0, per_frame))

    def snapshot(self) -> dict[str, float | int]:
        """导出报告所需的标量摘要。"""

        return {
            "peak": self._peak,
            "peak_dbfs": to_dbfs(self._peak, self.floor_dbfs),
            "rms": self.rms(),
            "rms_dbfs": to_dbfs(self.rms(), self.floor_dbfs),
            "effective_rms": self.effective_rms(),
            "effective_rms_dbfs": to_dbfs(self.effective_rms(), self.floor_dbfs),
            "clipped_samples": self._clipped,
            "frames": self._frames,
            "active_frames": self._active_frames,
        }


class TrackMetrics:
    """单轨混音前统计(峰值与削波,同样按块累加)。"""

    def __init__(self, clip_threshold: float = 1.0) -> None:
        self.clip_threshold = float(clip_threshold)
        self._peak = 0.0
        self._clipped = 0

    def add(self, contribution: np.ndarray) -> None:
        arr = np.asarray(contribution, dtype=np.float64)
        if arr.size == 0:
            return
        block_peak = float(np.max(np.abs(arr)))
        if block_peak > self._peak:
            self._peak = block_peak
        self._clipped += count_clipped(arr, self.clip_threshold)

    @property
    def peak(self) -> float:
        return self._peak

    @property
    def clipped_samples(self) -> int:
        return self._clipped
