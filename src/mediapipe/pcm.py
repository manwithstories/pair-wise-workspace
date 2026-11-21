"""PCM 验证样例：用 NumPy 生成确定性波形、soundfile 落盘与回读检查。

用于在不触碰压缩编解码器的前提下，独立验证时间基准与采样计数逻辑：
soundfile 负责 WAV/FLAC 等 PCM 容器的写入与读取，NumPy 负责波形生成与
数值断言。生成的 PCM 也可作为音频轨道内容的参考信号来源。
"""
from __future__ import annotations

import os

import numpy as np
import soundfile as sf

SAMPLE_RATE = 48000


def generate_reference_pcm(
    path: str, seconds: float = 1.0, freq: float = 440.0
) -> dict[str, int]:
    """生成双声道确定性正弦 PCM（WAV），返回采样数等元信息。"""
    n = int(SAMPLE_RATE * seconds)
    t = np.arange(n) / SAMPLE_RATE
    wave = (0.2 * np.sin(2 * np.pi * freq * t)).astype(np.float32)
    pcm = np.column_stack([wave, wave])
    sf.write(path, pcm, SAMPLE_RATE, subtype="FLOAT")
    return {
        "samples": n,
        "sample_rate": SAMPLE_RATE,
        "channels": 2,
        "bytes": os.path.getsize(path),
    }


def check_reference_pcm(path: str, expected_samples: int) -> dict[str, object]:
    """回读 PCM 并核对采样率、声道数、采样数与信号能量（容差内）。"""
    data, rate = sf.read(path, dtype="float32", always_2d=True)
    if rate != SAMPLE_RATE:
        raise ValueError(f"采样率不符: {rate} != {SAMPLE_RATE}")
    if data.shape[1] != 2:
        raise ValueError(f"声道数不符: {data.shape[1]} != 2")
    if data.shape[0] != expected_samples:
        raise ValueError(
            f"采样数不符: {data.shape[0]} != {expected_samples}"
        )
    rms = float(np.sqrt(np.mean(np.square(data))))
    if not (0.05 < rms < 0.5):
        raise ValueError(f"信号能量异常 RMS={rms}")
    return {
        "samples": int(data.shape[0]),
        "sample_rate": int(rate),
        "channels": int(data.shape[1]),
        "rms": round(rms, 6),
    }
