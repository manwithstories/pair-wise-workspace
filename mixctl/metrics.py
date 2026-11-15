"""音频指标：峰值、RMS、削波与自动化控制点统计。

所有累积都使用 float64 运行和。dBFS 以满幅 1.0 为 0 dB：
``db = 20 * log10(amplitude)``。没有能量的轨道（静音或被独奏排除）
RMS 输出 JSON null；峰值为 0 时 peak_dbfs 同样输出 null，避免 -Inf。
"""

from __future__ import annotations

import math

import numpy as np


class Metrics:
    def __init__(self, clip_threshold: float = 1.0):
        self.clip_threshold = float(clip_threshold)
        self.peak = 0.0
        self.sum_squares = 0.0
        self.samples = 0
        self.clip_samples = 0
        self.auto_points = 0

    def update(self, block: np.ndarray) -> None:
        data = np.asarray(block, dtype=np.float64)
        if data.size == 0:
            return
        absolute = np.abs(data)
        block_peak = float(absolute.max())
        if block_peak > self.peak:
            self.peak = block_peak
        self.sum_squares += float(np.sum(np.square(data, dtype=np.float64)))
        self.samples += int(data.size)
        self.clip_samples += int(np.count_nonzero(absolute >= self.clip_threshold))

    def add_auto_points(self, count: int) -> None:
        self.auto_points += int(count)

    @property
    def rms(self) -> float:
        if self.samples == 0 or self.sum_squares <= 0.0:
            return 0.0
        return math.sqrt(self.sum_squares / self.samples)

    @property
    def peak_dbfs(self) -> float | None:
        if self.peak <= 0.0:
            return None
        return 20.0 * math.log10(self.peak)

    @property
    def rms_dbfs(self) -> float | None:
        rms_value = self.rms
        if rms_value <= 0.0:
            return None
        return 20.0 * math.log10(rms_value)

    def to_report(self) -> dict:
        return {
            "peak_dbfs": self.peak_dbfs,
            "rms_dbfs": self.rms_dbfs,
            "clip_samples": self.clip_samples,
            "auto_points": self.auto_points,
        }
