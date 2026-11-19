"""节点漂移估计与音视频时间基准映射。

公共时间基准为音频采样时钟：以音频包的 (媒体 PTS, 节点到达墙钟)
锚点做最小二乘拟合，斜率即节点相对媒体时钟的漂移率，截距把任意
轨道的媒体时间戳映射到公共时间线。视频轨通过自身锚点估计与音频
基准之间的固定偏移与漂移，从而支持可变帧率按实际帧时间戳对齐。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .model import TrackKind


@dataclass
class DriftModel:
    """单条轨道的线性映射 common_us = slope * pts_us + intercept。"""

    track: TrackKind
    slope: float = 1.0
    intercept_us: float = 0.0
    drift_ppm: float = 0.0
    samples: int = field(default=0, init=False)
    _pts: list[int] = field(default_factory=list, init=False)
    _wall: list[int] = field(default_factory=list, init=False)

    def add_anchor(self, pts_us: int, wall_us: int, max_points: int = 4096) -> None:
        self._pts.append(pts_us)
        self._wall.append(wall_us)
        if len(self._pts) > max_points:  # 滑动窗口控制长直播内存
            self._pts = self._pts[max_points // 2:]
            self._wall = self._wall[max_points // 2:]
        self.samples = len(self._pts)

    def fit(self) -> bool:
        """最小二乘拟合；锚点不足或时间跨度为零时返回 False。"""
        if len(self._pts) < 2:
            return False
        x = np.asarray(self._pts, dtype=np.float64)
        y = np.asarray(self._wall, dtype=np.float64)
        span = x[-1] - x[0]
        if span <= 0:
            return False
        slope, intercept = np.polyfit(x, y, 1)
        if not np.isfinite(slope) or not np.isfinite(intercept):
            return False
        self.slope = float(slope)
        self.intercept_us = float(intercept)
        self.drift_ppm = (self.slope - 1.0) * 1_000_000.0
        return True


class ClockDomain:
    """音频基准时钟 + 视频轨映射的组合域。"""

    def __init__(self, jitter_tolerance_us: int = 5_000) -> None:
        self.audio = DriftModel(TrackKind.AUDIO)
        self.video = DriftModel(TrackKind.VIDEO)
        # 视频相对音频的公共时间线偏移（us），由双轨锚点对齐得到
        self.video_offset_us: int = 0
        self._tolerance = jitter_tolerance_us
        self._aligned = False

    def anchor(self, track: TrackKind, pts_us: int, wall_us: int) -> None:
        (self.audio if track == TrackKind.AUDIO else self.video).add_anchor(
            pts_us, wall_us)

    def estimate(self) -> dict[str, float]:
        """拟合漂移并对齐双轨，返回供指标/日志使用的摘要。"""
        audio_ok = self.audio.fit()
        video_ok = self.video.fit()
        if audio_ok and video_ok and self.video.samples >= 2:
            # 取两轨各自首个锚点在墙钟上的位置差作为偏移估计
            a0 = self.audio.slope * self.audio._pts[0] + self.audio.intercept_us
            v0 = self.video.slope * self.video._pts[0] + self.video.intercept_us
            self.video_offset_us = int(round(v0 - a0))
            self._aligned = True
        return {
            "audio_drift_ppm": round(self.audio.drift_ppm, 3),
            "video_drift_ppm": round(self.video.drift_ppm, 3),
            "video_offset_us": self.video_offset_us,
            "audio_anchors": self.audio.samples,
            "video_anchors": self.video.samples,
            "aligned": self._aligned,
            "fit_ok": audio_ok,
        }

    def to_common(self, track: TrackKind, pts_us: int) -> int:
        """把轨道媒体时间戳映射到音频公共时间线（us 整数）。"""
        if track == TrackKind.AUDIO:
            return pts_us
        return pts_us + self.video_offset_us

    def wall_estimate(self, pts_us: int) -> int:
        """音频基准下的预期墙钟，用于缓冲延迟计算。"""
        return int(round(self.audio.slope * pts_us + self.audio.intercept_us))
