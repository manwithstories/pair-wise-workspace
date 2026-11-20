"""音频探针：用 soundfile 独立校验音频夹具与时间线连续性。

PyAV 负责容器解复用，soundfile 提供与编码器无关的第二读取路径，
媒体工程师可用它确认恢复后的音频采样率、时长与采样连续性，避免
单一解码库的自洽性盲区。仅依赖 soundfile + numpy，不引入新第三方。
"""

from __future__ import annotations

import math
import os
from dataclasses import dataclass

import numpy as np
import soundfile as sf


@dataclass
class AudioProbeResult:
    frames: int
    sample_rate: int
    channels: int
    duration_s: float
    rms: float
    contiguous: bool
    format: str


def write_reference_wav(path: str, samples: np.ndarray,
                        sample_rate: int) -> None:
    """把音频采样写为 WAV 参考文件，供独立探针回读。"""
    sf.write(path, np.asarray(samples, dtype=np.float32),
             sample_rate, subtype="FLOAT")


def probe_wav(path: str) -> AudioProbeResult:
    """读取 WAV 并校验基本属性与采样连续性（无 NaN/Inf、时长合理）。"""
    if not os.path.isfile(path):
        raise FileNotFoundError(path)
    data, sr = sf.read(path, always_2d=True, dtype="float32")
    frames, channels = data.shape
    finite = bool(np.isfinite(data).all())
    rms = float(np.sqrt(np.mean(data.astype(np.float64) ** 2)))
    duration = frames / sr if sr else 0.0
    contiguous = finite and frames > 0 and math.isfinite(duration)
    return AudioProbeResult(
        frames=frames, sample_rate=sr, channels=channels,
        duration_s=round(duration, 6), rms=round(rms, 6),
        contiguous=contiguous, format=sf.info(path).format)


def verify_timeline_continuity(pts_us: list[int],
                               max_gap_us: int = 200_000) -> bool:
    """检查音频 PTS 序列单调且相邻间隔不超过容忍上限。"""
    if len(pts_us) < 2:
        return True
    arr = np.asarray(pts_us, dtype=np.int64)
    deltas = np.diff(arr)
    return bool((deltas >= 0).all() and deltas.max() <= max_gap_us)
