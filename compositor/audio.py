"""确定性 PCM 夹具生成与音频事件采样分析。

不写任何 WAV 文件: PCM 只存在于内存(可选封装到内存中的 WAVE 容器),
满足"结果仅落盘 JSON/JSONL"的约束。所有波形由事件序号与采样点的
确定性公式生成, 重复运行逐字节一致。
"""

from __future__ import annotations

import io
import math
import wave

import numpy as np

from .types import (
    AudioEvent,
    AudioEventAnalysis,
    CompositorError,
    ErrorType,
    Timeline,
    frame_to_second,
    sample_to_second,
    second_to_sample,
    us,
)

# 事件序号 -> 整数基频(Hz), 全部落在常见音高上, 避免浮点常量漂移。
_BASE_FREQS = (220, 277, 330, 415, 494, 554, 659, 740)


def event_tone(event_index: int, sample_index: int, length_samples: int,
               sample_rate: int) -> np.ndarray:
    """生成单个事件的确定性波形: 正弦音 + 首尾各 8% 线性淡入淡出。"""
    freq = _BASE_FREQS[event_index % len(_BASE_FREQS)]
    t = np.arange(sample_index, sample_index + length_samples, dtype=np.float64)
    phase = 2.0 * math.pi * freq * (t / sample_rate)
    fade = max(1, length_samples // 12)
    ramp_up = np.minimum(1.0, np.arange(length_samples) / fade)
    ramp_down = np.minimum(
        1.0, (length_samples - 1 - np.arange(length_samples)) / fade)
    envelope = np.minimum(ramp_up, ramp_down)
    return 0.5 * envelope * np.sin(phase)


def synthesize_pcm(timeline: Timeline) -> np.ndarray:
    """把全部音频事件混合为单声道 float64 PCM, 长度覆盖整条时间线。

    事件重叠时电平相加后 tanh 软限幅, 不引入任何随机量。
    """
    total_samples = second_to_sample(timeline.duration, timeline.sample_rate)
    pcm = np.zeros(total_samples, dtype=np.float64)
    for index, event in enumerate(timeline.audio_events):
        first = second_to_sample(event.start, timeline.sample_rate)
        last = second_to_sample(event.end, timeline.sample_rate) - 1
        if first > last:
            raise CompositorError(
                f"音频事件 {event.event_id!r} 短于一个采样点",
                ErrorType.AUDIO_FAILED)
        if last >= total_samples or first < 0:
            raise CompositorError(
                f"音频事件 {event.event_id!r} 采样范围 [{first}, {last}] 越界",
                ErrorType.AUDIO_FAILED)
        pcm[first:last + 1] += event_tone(
            index, 0, last - first + 1, timeline.sample_rate)
    return np.tanh(pcm)


def pcm_to_wav_bytes(pcm: np.ndarray, sample_rate: int) -> bytes:
    """把 PCM 封装为 16-bit 单声道 WAVE 字节流(仅内存, 不落盘)。"""
    pcm16 = np.round(np.clip(pcm, -1.0, 1.0) * 32767.0).astype("<i2")
    buffer = io.BytesIO()
    with wave.open(buffer, "wb") as wav:
        wav.setnchannels(1)
        wav.setsampwidth(2)
        wav.setframerate(sample_rate)
        wav.writeframes(pcm16.tobytes())
    return buffer.getvalue()


def analyze_event(event: AudioEvent, timeline: Timeline) -> AudioEventAnalysis:
    """计算单个事件的首/末采样点、末帧时间与音画对齐误差(微秒)。"""
    rate = timeline.sample_rate
    fps = timeline.fps
    first_sample = second_to_sample(event.start, rate)
    last_sample = second_to_sample(event.end, rate) - 1
    if first_sample < 0 or last_sample >= second_to_sample(
            timeline.duration, rate):
        raise CompositorError(
            f"音频事件 {event.event_id!r} 采样范围越界",
            ErrorType.AUDIO_FAILED)
    # 结束采样点对应时刻所在帧(即末帧时间的帧号)。
    end_moment = sample_to_second(last_sample + 1, rate)
    end_frame = (end_moment * fps).numerator // (end_moment * fps).denominator
    # 起始点到最近帧边界的距离(有理数精确计算, 再转整数微秒)。
    nearest_frame = (event.start * fps).numerator // \
        (event.start * fps).denominator
    boundary = frame_to_second(nearest_frame, fps)
    error_us = us(abs(event.start - boundary))
    return AudioEventAnalysis(
        event_id=event.event_id,
        start_second_numerator=event.start.numerator,
        start_second_denominator=event.start.denominator,
        first_sample=first_sample,
        last_sample=last_sample,
        end_frame=end_frame,
        av_sync_error_us=error_us,
    )


def analyze_audio(timeline: Timeline) -> list[AudioEventAnalysis]:
    """分析全部事件, 并保证结果顺序与时间线声明一致。"""
    return [analyze_event(event, timeline) for event in timeline.audio_events]


def frame_start_sample(frame_index: int, timeline: Timeline) -> int:
    """某视频帧起始时刻对应的采样点序号。"""
    return second_to_sample(frame_to_second(frame_index, timeline.fps),
                            timeline.sample_rate)
