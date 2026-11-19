"""音轨：合成、整数刻度偏移与峰值/RMS/静音统计。"""

from __future__ import annotations

import hashlib

import numpy as np
import soundfile as sf

from .timebase import TimeBase

SAMPLE_RATE = 48_000
SILENCE_THRESHOLD = 1e-3
MIN_SILENCE_S = 0.05


def synth_audio(track_id: str, duration_ticks: int, tb: TimeBase,
                sample_rate: int = SAMPLE_RATE) -> np.ndarray:
    """确定性合成音轨：双正弦叠加 + 低幅噪声，含一段静音。"""
    seed = int.from_bytes(
        hashlib.sha256(track_id.encode()).digest()[:4], "big"
    )
    rng = np.random.default_rng(seed)
    n = int(round(tb.ticks_to_seconds(duration_ticks) * sample_rate))
    t = np.arange(n, dtype=np.float64) / sample_rate
    f1 = 220.0 + (seed % 7) * 40.0
    f2 = 440.0 + (seed % 5) * 60.0
    sig = 0.3 * np.sin(2 * np.pi * f1 * t) + 0.2 * np.sin(2 * np.pi * f2 * t)
    sig += 0.01 * rng.standard_normal(n)
    # 中段插入 0.2s 静音，供静音区间统计
    gap = int(0.2 * sample_rate)
    mid = n // 2
    if n > gap * 4:
        sig[mid : mid + gap] = 0.0
    return sig.astype(np.float32)


def apply_offset(samples: np.ndarray, offset_ticks: int, tb: TimeBase,
                 sample_rate: int = SAMPLE_RATE) -> np.ndarray:
    """按整数刻度偏移音轨：正值延迟（前补零），负值提前（截头）。"""
    shift = int(round(tb.ticks_to_seconds(offset_ticks) * sample_rate))
    if shift > 0:
        return np.concatenate([np.zeros(shift, dtype=samples.dtype), samples])
    if shift < 0:
        return samples[-shift:] if -shift < len(samples) else samples[:0]
    return samples


def audio_stats(samples: np.ndarray, sample_rate: int = SAMPLE_RATE) -> dict:
    """峰值、RMS 与静音区间（毫秒）。"""
    if len(samples) == 0:
        return {"peak": 0.0, "rms": 0.0, "silence_intervals_ms": []}
    peak = float(np.max(np.abs(samples)))
    rms = float(np.sqrt(np.mean(samples.astype(np.float64) ** 2)))
    silent = np.abs(samples) < SILENCE_THRESHOLD
    min_len = int(MIN_SILENCE_S * sample_rate)
    intervals: list[list[float]] = []
    start = -1
    for i, flag in enumerate(np.append(silent, False)):
        if flag and start < 0:
            start = i
        elif not flag and start >= 0:
            if i - start >= min_len:
                intervals.append(
                    [start * 1000.0 / sample_rate, i * 1000.0 / sample_rate]
                )
            start = -1
    return {"peak": peak, "rms": rms, "silence_intervals_ms": intervals}


def write_wav(path, samples: np.ndarray, sample_rate: int = SAMPLE_RATE) -> None:
    sf.write(str(path), samples, sample_rate, subtype="PCM_16")
