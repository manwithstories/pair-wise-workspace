"""PCM/WAV 连续静音区间检测，并映射到整数微秒统一时间轴。

支持：多声道合并（平均能量）、短静音门限（最短持续时长）、
文件尾截断（按实际可读样本计算时长）。所有区间端点以整数微秒输出，
与视频时间线共用同一时间基准（timeline.US_PER_SECOND）。
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

import numpy as np
import soundfile as sf

from .timeline import ErrorKind, MediaError, Stage, samples_to_us


@dataclass(slots=True)
class SilenceInterval:
    start_us: int
    end_us: int
    start_sample: int
    end_sample: int
    peak_amplitude: float

    def to_dict(self) -> dict:
        return {
            "start_us": self.start_us,
            "end_us": self.end_us,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "peak_amplitude": self.peak_amplitude,
        }


@dataclass(slots=True)
class SilenceResult:
    sample_rate: int
    channels: int
    total_samples: int
    duration_us: int
    truncated: bool
    intervals: list[SilenceInterval] = field(default_factory=list)
    errors: list[MediaError] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "total_samples": self.total_samples,
            "duration_us": self.duration_us,
            "truncated": self.truncated,
            "intervals": [i.to_dict() for i in self.intervals],
            "errors": [e.to_dict() for e in self.errors],
        }


def _merge_channels(samples: np.ndarray) -> np.ndarray:
    """多声道按平均能量合并为单声道幅度包络，返回 |amplitude|。"""

    if samples.ndim == 1:
        return np.abs(samples)
    return np.max(np.abs(samples), axis=1)


def detect_silence(
    path: str,
    *,
    threshold_db: float = -45.0,
    min_duration_us: int = 50_000,
    expected_duration_us: int | None = None,
) -> SilenceResult:
    """检测静音区间。

    threshold_db: 低于该峰值幅度视为静音样本；
    min_duration_us: 短于此门限的静音间隙被忽略；
    expected_duration_us: 给定且大于实际时长时判定为文件尾截断。
    """

    if not os.path.exists(path):
        raise FileNotFoundError(path)
    data, sample_rate = sf.read(path, always_2d=False, dtype="float32")
    channels = 1 if data.ndim == 1 else data.shape[1]
    envelope = _merge_channels(data)
    total = envelope.shape[0]
    duration_us = samples_to_us(total, sample_rate)

    errors: list[MediaError] = []
    truncated = False
    if expected_duration_us is not None and duration_us < expected_duration_us:
        truncated = True
        errors.append(MediaError(
            Stage.SILENCE, ErrorKind.TRUNCATED_UNIT, -1,
            f"音频实际时长 {duration_us}us 短于预期 {expected_duration_us}us，"
            "按文件尾截断处理", recovered=False,
            context={"actual_us": duration_us,
                     "expected_us": expected_duration_us}))

    amplitude_floor = 10.0 ** (threshold_db / 20.0)
    is_silent = envelope <= amplitude_floor
    min_samples = max(
        1, (min_duration_us * sample_rate + 999_999) // 1_000_000)

    intervals: list[SilenceInterval] = []
    idx = 0
    while idx < total:
        if not is_silent[idx]:
            idx += 1
            continue
        start = idx
        while idx < total and is_silent[idx]:
            idx += 1
        end = idx  # 独占
        if end - start < min_samples:
            continue
        peak = float(envelope[start:end].max()) if end > start else 0.0
        intervals.append(SilenceInterval(
            start_us=samples_to_us(start, sample_rate),
            end_us=samples_to_us(end, sample_rate),
            start_sample=start,
            end_sample=end,
            peak_amplitude=peak,
        ))

    return SilenceResult(
        sample_rate=sample_rate,
        channels=channels,
        total_samples=total,
        duration_us=duration_us,
        truncated=truncated,
        intervals=intervals,
        errors=errors,
    )
