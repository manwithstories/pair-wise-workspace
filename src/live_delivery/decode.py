"""PyAV 流式解封装与解码，输出 float32 平面音频块。

输入分片中途的采样率/声道数变化按分片级处理：每个分片独立读取
其真实流参数，由下游 audio 模块统一归一到目标规格。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Iterator

import av
import numpy as np

from .errors import DecodeError
from .transport import Delivery


@dataclass
class AudioChunk:
    """解码产出的一段音频（统一媒体时间基准）。"""

    data: np.ndarray       # (channels, samples) float32
    rate: int              # 该块的原始采样率
    layout: str            # 该块的原始声道布局名
    sequence: int          # 来源分片序号（静音填充块为 -1）
    media_start: float     # 块起点的清单媒体时间（秒）

    @property
    def samples(self) -> int:
        return int(self.data.shape[1])

    @property
    def nbytes(self) -> int:
        return int(self.data.nbytes)


def decode(delivery: Delivery) -> Iterator[AudioChunk]:
    """流式解码单个分片；任何失败抛出 DecodeError（由上游按策略处理）。"""
    ref = delivery.ref
    try:
        container = av.open(str(delivery.path))
    except Exception as exc:
        raise DecodeError(f"无法解封装 {ref.uri}: {exc}") from exc
    with container:
        if not container.streams.audio:
            raise DecodeError(f"分片无音频流: {ref.uri}")
        stream = container.streams.audio[0]
        resampler: av.AudioResampler | None = None
        resample_key: tuple[int, str] | None = None
        offset = 0.0
        try:
            for frame in container.decode(stream):
                key = (int(frame.sample_rate), frame.layout.name)
                if resample_key != key:
                    # 输入中途采样率/声道数变化：重建重采样器
                    resampler = av.AudioResampler(
                        format="fltp", layout=frame.layout, rate=frame.sample_rate
                    )
                    resample_key = key
                try:
                    out_frames = resampler.resample(frame)
                except Exception as exc:
                    raise DecodeError(f"格式转换失败 {ref.uri}: {exc}") from exc
                for out in out_frames:
                    arr = np.asarray(out.to_ndarray(), dtype=np.float32)
                    if arr.ndim == 1:
                        arr = arr[np.newaxis, :]
                    rate = key[0]
                    yield AudioChunk(
                        arr, rate, key[1], ref.sequence, ref.start + offset
                    )
                    offset += arr.shape[1] / rate
        except DecodeError:
            raise
        except Exception as exc:
            raise DecodeError(f"解码失败 {ref.uri}: {exc}") from exc
