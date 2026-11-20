"""媒体时间基准与关键帧探测（PyAV），及确定性夹具生成（soundfile + PyAV）。

探测侧只读取容器/流元数据与帧的 key_frame 标记，不做像素解码；
生成侧用固定 GOP 与零噪声帧保证同种子逐字节/逐帧可重复，
音频夹具由 soundfile 按确定性正弦混合写入 WAV。
"""
from __future__ import annotations

from fractions import Fraction
from pathlib import Path

import av
import numpy as np
import soundfile as sf

from .model import Keyframe, MediaInfo
from .timecode import parse_rate


def probe_media(path: str | Path) -> MediaInfo:
    """读取媒体帧率、时长与全部关键帧位置（统一有理数时间基准）。"""

    path = Path(path)
    container = av.open(str(path))
    try:
        video = container.streams.video[0]
        rate = Fraction(video.average_rate)
        time_base = Fraction(video.time_base)
        keyframes: list[Keyframe] = []
        index = 0
        for frame in container.decode(video):
            if frame.key_frame:
                keyframes.append(
                    Keyframe(
                        index=index,
                        pts_time=Fraction(frame.pts, 1) * time_base,
                        is_key=True,
                    )
                )
            index += 1
        duration = Fraction(video.duration, 1) * time_base \
            if video.duration else Fraction(index, rate)
        return MediaInfo(
            source=path.name,
            frame_rate=rate,
            duration=duration,
            keyframes=keyframes,
        )
    finally:
        container.close()


def make_audio_fixture(
    path: str | Path,
    frames: int,
    rate_fps: Fraction,
    audio_rate: int = 48_000,
    seed: int = 42,
) -> None:
    """生成确定性单声道 WAV：时长与视频帧数对齐，波形为固定种子正弦混合。"""

    duration = float(Fraction(frames, rate_fps))
    n = int(round(duration * audio_rate))
    t = np.arange(n, dtype=np.float64) / audio_rate
    rng = np.random.default_rng(seed)
    signal = np.zeros(n, dtype=np.float64)
    for harmonic in range(1, 4):
        amp = 0.25 / harmonic
        freq = 220.0 * harmonic * (1 + rng.uniform(-0.01, 0.01))
        signal += amp * np.sin(2 * np.pi * freq * t)
    peak = np.max(np.abs(signal)) or 1.0
    sf.write(str(path), (signal / peak).astype(np.float32), audio_rate,
             subtype="PCM_16")


def make_video_fixture(
    path: str | Path,
    frames: int,
    rate_fps: str | Fraction = "24",
    gop: int = 100,
    width: int = 160,
    height: int = 120,
) -> MediaInfo:
    """生成确定性 H.264 视频夹具：固定 GOP，每 gop 帧一个关键帧。"""

    rate = parse_rate(rate_fps)
    container = av.open(str(path), "w")
    try:
        stream = container.add_stream("h264", rate=int(float(rate)))
        stream.width, stream.height = width, height
        stream.pix_fmt = "yuv420p"
        stream.gop_size = gop
        stream.options = {"preset": "veryfast", "repeat-headers": "1"}
        rng = np.random.default_rng(0)
        base = rng.integers(0, 255, size=(height, width, 3), dtype=np.uint8)
        for i in range(frames):
            arr = (base.astype(np.uint16) + i) % 256
            frame = av.VideoFrame.from_ndarray(
                arr.astype(np.uint8), format="rgb24"
            )
            frame.pts = i
            for packet in stream.encode(frame):
                container.mux(packet)
        for packet in stream.encode():
            container.mux(packet)
    finally:
        container.close()
    return probe_media(path)
