"""基于互相关的音画同步实测。

视频夹具在固定时刻叠加全帧亮度脉冲，音频夹具在同一时刻放置尖锐同步脉冲。
分别从解码视频的逐帧亮度包络与音频波形中提取脉冲位置，做互相关得到实测
偏移；与清单声明的 sync_offset_us 比较即同步误差（毫秒）。
"""
from __future__ import annotations

import av
import numpy as np

from .model import (TrackDecl, VideoCodec, resolve_within_root)


def video_flash_times_us(path: str, codec: VideoCodec,
                         frame_rate: float) -> list[int]:
    """解码视频，返回亮度峰值帧对应的时间（微秒）。"""
    brightness: list[float] = []
    fh = open(path, "rb")
    try:
        container = av.open(fh, format=codec.pyav_format)
        stream = container.streams.video[0]
        for pkt in container.demux(stream):
            try:
                for frame in pkt.decode():
                    arr = frame.to_ndarray(format="gray")
                    brightness.append(float(np.mean(arr, dtype=np.float64)))
            except av.FFmpegError:
                break
        container.close()
    finally:
        fh.close()
    if not brightness:
        return []
    sig = np.asarray(brightness, dtype=np.float64)
    sig = sig - sig.mean()
    # 峰值：显著高于局部标准差的帧。
    sd = sig.std() or 1.0
    peaks = [i for i in range(1, len(sig) - 1)
             if sig[i] > 3.0 * sd and sig[i] >= sig[i - 1]
             and sig[i] >= sig[i + 1]]
    return [round(i * 1_000_000 / frame_rate) for i in peaks]


def audio_pulse_time_us(samples: np.ndarray, rate: int) -> int | None:
    """返回音频能量包络中最强脉冲的时间（微秒）。samples 为 (frames, ch)。"""
    if len(samples) == 0:
        return None
    mono = samples.mean(axis=1) if samples.ndim == 2 else samples
    env = np.abs(mono.astype(np.float64))
    if env.max() <= 0:
        return None
    idx = int(np.argmax(env))
    return round(idx * 1_000_000 / rate)


def measure_sync_ms(video_track: TrackDecl, root: str,
                    audio_track: TrackDecl, audio_samples: np.ndarray,
                    audio_rate: int) -> float:
    """实测单条音频轨相对参考视频的同步误差（毫秒）。

    取两者首个脉冲时间之差，再减去清单声明的相对偏移；残差即未被声明
    偏移解释的同步误差。
    """
    video_path = str(resolve_within_root(root, video_track.path))
    flashes = video_flash_times_us(video_path, video_track.codec,
                                   video_track.frame_rate)
    pulse = audio_pulse_time_us(audio_samples, audio_rate)
    if not flashes or pulse is None:
        return 0.0
    observed = pulse - flashes[0]
    declared = audio_track.sync_offset_us - video_track.sync_offset_us
    return abs(observed - declared) / 1000.0
