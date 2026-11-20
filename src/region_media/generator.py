"""本地样例夹具生成（上）：PyAV 媒体编码与传输场景构造。

不引用任何外部文件：音频为带时间标记的正弦波，视频为帧号烧录
图案；编码参数刻意取小分辨率/短时长，保证夹具轻量且单片不超过
:data:`MAX_SEGMENT_BYTES`。

场景（:class:`ScenarioPlan`）：
    normal / reorder / loss / wrap / drift / vfr / truncated / corrupt
全部基于确定性种子生成，verify 套件与 process 命令共用同一夹具。
"""

from __future__ import annotations

import math
import os
from dataclasses import dataclass, field

import av
import numpy as np

from .model import MAX_SEGMENT_BYTES, TrackKind

SAMPLE_RATE = 48000
VIDEO_W, VIDEO_H = 160, 120
SEG_SECONDS = 1.0
FPS_NOMINAL = 30

SCENARIOS = ("normal", "reorder", "loss", "wrap", "drift",
             "vfr", "truncated", "corrupt")


@dataclass
class SegmentSpec:
    """单个分片的生成参数。"""

    seq: int
    kind: TrackKind
    uri: str
    scenario: str
    seed: int
    frames: int = FPS_NOMINAL
    vfr: bool = False
    corrupt: bool = False


@dataclass
class ScenarioPlan:
    name: str
    segments: int = 6
    seed: int = 13812
    vfr: bool = False
    corrupt_seg: int | None = None
    extra: dict = field(default_factory=dict)


def _tone(seq: int, seconds: float) -> np.ndarray:
    """带分片标记的立体声正弦波，频率随序号变化便于连续性核验。"""
    n = int(seconds * SAMPLE_RATE)
    t = np.arange(n, dtype=np.float32) / SAMPLE_RATE
    freq = 220.0 + 40.0 * (seq % 8)
    left = 0.35 * np.sin(2 * math.pi * freq * t)
    right = 0.35 * np.sin(2 * math.pi * (freq * 1.5) * t)
    return np.ascontiguousarray(np.stack([left, right]))


def _frame_image(seq: int, frame: int, vfr: bool) -> np.ndarray:
    img = np.zeros((VIDEO_H, VIDEO_W, 3), dtype=np.uint8)
    stripe = 8 + (frame % 5) * (2 if vfr else 1)
    img[:, ::stripe] = (180 + (seq * 17) % 60, 120, 200)
    img[:12, : 6 + frame % 20] = (255, 255, 255)
    return img


def _vfr_pts(frames: int) -> list[int]:
    """可变帧率帧时间戳（us）：在标称 33.3ms 上叠加 ±30% 确定性抖动。"""
    rng = np.random.default_rng(7)
    deltas = (1_000_000 / FPS_NOMINAL) * (
        1.0 + 0.3 * rng.uniform(-1, 1, size=frames))
    pts = [0]
    for d in deltas[:-1]:
        pts.append(int(pts[-1] + d))
    return pts


def encode_segment(spec: SegmentSpec) -> bytes:
    """把一个分片编码为 matroska 字节串；corrupt 时截断尾部模拟坏包。"""
    buf = _MemoryTarget()
    container = av.open(buf, mode="w", format="matroska")
    audio = container.add_stream("aac", rate=SAMPLE_RATE)
    audio.layout = "stereo"
    video = container.add_stream("h264", rate=FPS_NOMINAL)
    video.width, video.height = VIDEO_W, VIDEO_H
    video.pix_fmt = "yuv420p"
    video.options = {"preset": "ultrafast", "tune": "zerolatency"}

    duration_us = int(SEG_SECONDS * 1_000_000)
    audio_frames = int(SEG_SECONDS * SAMPLE_RATE / 1024)
    # 33 位时间戳回绕不在媒体编码层构造（独立容器会被复用器重定位
    # 时基），而由 ingest.NetworkConditioner 在传输注入层模拟节点
    # 计数器翻转；这里各分片统一使用从 0 开始的连续时间戳。
    base_pts = spec.seq * SAMPLE_RATE
    for i in range(audio_frames):
        wave = _tone(spec.seq, 1024 / SAMPLE_RATE)
        frame = av.AudioFrame.from_ndarray(wave, format="fltp",
                                           layout="stereo")
        frame.sample_rate = SAMPLE_RATE
        frame.pts = base_pts + i * 1024
        for pkt in audio.encode(frame):
            container.mux(pkt)

    vfr_pts = _vfr_pts(spec.frames) if spec.vfr else None
    for f in range(spec.frames):
        img = _frame_image(spec.seq, f, spec.vfr)
        frame = av.VideoFrame.from_ndarray(img, format="rgb24").reformat(
            format="yuv420p")
        if vfr_pts is not None:
            frame.pts = spec.seq * duration_us + vfr_pts[f]
            frame.time_base = "1/1000000"
        else:
            frame.pts = spec.seq * FPS_NOMINAL + f
            frame.time_base = f"1/{FPS_NOMINAL}"
        for pkt in video.encode(frame):
            container.mux(pkt)
    for pkt in audio.encode(None):
        container.mux(pkt)
    for pkt in video.encode(None):
        container.mux(pkt)
    container.close()
    data = buf.getvalue()
    if spec.corrupt:
        data = _corrupt_matroska(data)
    if len(data) > MAX_SEGMENT_BYTES:
        raise RuntimeError(f"分片 {spec.uri} 超过字节上限")
    return data


def _corrupt_matroska(data: bytes) -> bytes:
    """破坏 EBML 头并截断尾部，模拟跨节点传输后的损坏媒体包。

    实测 PyAV 对 matroska 簇内坏块倾向静默跳过，而 EBML 头损坏会让
    ``av.open`` 稳定抛出 InvalidDataError，由 ingest 记为
    CORRUPT_PACKET；尾部截断模拟分片未传完整。
    """
    out = bytearray(data)
    # EBML 头位于文件起始 40 字节内，破坏其声明字段但保留首字节魔数
    lo = min(5, len(out) - 1)
    hi = min(40, len(out))
    out[lo:hi] = b"\xff" * (hi - lo)
    del out[int(len(out) * 0.8):]
    return bytes(out)


class _MemoryTarget:
    """PyAV 可写文件对象；支持回 seek 以满足 matroska 头部回写。"""

    def __init__(self) -> None:
        self._buf = bytearray()
        self._pos = 0

    def write(self, data: bytes) -> int:
        end = self._pos + len(data)
        if end > len(self._buf):
            self._buf.extend(b"\x00" * (end - len(self._buf)))
        self._buf[self._pos:end] = data
        self._pos = end
        return len(data)

    def seek(self, offset: int, whence: int = 0) -> int:
        if whence == 0:
            self._pos = offset
        elif whence == 1:
            self._pos += offset
        elif whence == 2:
            self._pos = len(self._buf) + offset
        return self._pos

    def tell(self) -> int:
        return self._pos

    def getvalue(self) -> bytes:
        return bytes(self._buf)


def build_plan(name: str, seed: int = 13812) -> ScenarioPlan:
    """场景名 -> 生成计划，参数与 NetworkProfile 在 pipeline 中对应。"""
    table = {
        "normal": ScenarioPlan("normal", seed=seed),
        "reorder": ScenarioPlan("reorder", seed=seed + 1,
                                extra={"reorder_rate": 0.35}),
        "loss": ScenarioPlan("loss", seed=seed + 2,
                             extra={"loss_rate": 0.12, "burst_loss_max": 3}),
        "wrap": ScenarioPlan("wrap", seed=seed + 3,
                             extra={"wrap_at_packet": 3}),
        "drift": ScenarioPlan("drift", seed=seed + 4,
                              extra={"drift_ppm": 1200.0}),
        "vfr": ScenarioPlan("vfr", seed=seed + 5, vfr=True),
        "truncated": ScenarioPlan("truncated", segments=4, seed=seed + 6,
                                  extra={"truncate_manifest": True}),
        "corrupt": ScenarioPlan("corrupt", seed=seed + 7, corrupt_seg=2),
    }
    if name not in table:
        raise KeyError(f"未知场景 {name}，可选: {', '.join(SCENARIOS)}")
    return table[name]


def write_segment_file(path: str, spec: SegmentSpec) -> int:
    data = encode_segment(spec)
    with open(path, "wb") as fh:
        fh.write(data)
    return len(data)
