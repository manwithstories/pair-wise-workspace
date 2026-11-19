"""渲染：PyAV 离屏编解码、多轨合成与帧序列导出。

源素材由确定性合成器生成（source_frame），编码后经包层传输、
修复，再离屏解码并合成。导出的每帧带帧序号、媒体时间戳（整数
刻度）与 SHA-256 哈希；解码失败时报告首个失败位置。
"""

from __future__ import annotations

import hashlib
import io
from dataclasses import dataclass

import av
import numpy as np

from .errors import DecodeError
from .timebase import TimeBase
from .timeline import FILL_ABORT, Timeline

PIX_FMT = "yuv420p"


def source_frame(source_id: str, source_ticks: int, tb: TimeBase,
                 width: int, height: int) -> np.ndarray:
    """确定性合成源帧：移动渐变 + 源标识色块。"""
    seed = int.from_bytes(
        hashlib.sha256(source_id.encode()).digest()[:4], "big"
    )
    frame_no = source_ticks // max(tb.frame_ticks(24), 1)
    y, x = np.mgrid[0:height, 0:width]
    base = np.array(
        [(seed >> 16) & 0xFF, (seed >> 8) & 0xFF, seed & 0xFF], dtype=np.float64
    )
    phase = (frame_no * 7 + seed) % 256
    grad = ((x + phase) % 256)[..., None].astype(np.float64)
    img = (base[None, None, :] * 0.6 + grad * 0.4).astype(np.uint8)
    block = 12 + (seed % 5)
    mask = ((x // block) + (y // block) + frame_no) % 2 == 0
    img[mask] = np.clip(img[mask].astype(int) + 40, 0, 255).astype(np.uint8)
    # 确定性细噪声：模拟真实传感器底噪，也让码率控制有实际负载
    rng = np.random.default_rng((seed, frame_no))
    noise = rng.integers(-16, 17, size=img.shape)
    img = np.clip(img.astype(int) + noise, 0, 255).astype(np.uint8)
    return np.ascontiguousarray(img)


def encode_source(path, source_id: str, frame_count: int, fps: int,
                  size: tuple[int, int], tb: TimeBase,
                  bit_rate: int, start_frame: int = 0) -> None:
    """把合成源编码为视频文件（传输前的原始段）。"""
    width, height = size
    container = av.open(str(path), mode="w")
    try:
        stream = container.add_stream("libx264", rate=fps)
        stream.width, stream.height = width, height
        stream.pix_fmt = PIX_FMT
        stream.bit_rate = bit_rate
        stream.bit_rate_tolerance = max(bit_rate // 20, 10_000)
        stream.options = {"preset": "medium"}
        frame_ticks = tb.frame_ticks(fps)
        for i in range(frame_count):
            frame_no = start_frame + i
            arr = source_frame(source_id, frame_no * frame_ticks, tb,
                               width, height)
            frame = av.VideoFrame.from_ndarray(arr, format="rgb24")
            frame.pts = frame_no
            for packet in stream.encode(frame):
                container.mux(packet)
        for packet in stream.encode():
            container.mux(packet)
    finally:
        container.close()


@dataclass
class DecodedFrame:
    index: int
    pts_ticks: int
    array: np.ndarray

    @property
    def digest(self) -> str:
        return hashlib.sha256(self.array.tobytes()).hexdigest()


def decode_frames(path, expected_frames: int, tb: TimeBase,
                  fps: int) -> list[DecodedFrame]:
    """离屏解码；失败或帧数不足时报告首个失败位置。"""
    frame_ticks = tb.frame_ticks(fps)
    out: list[DecodedFrame] = []
    try:
        container = av.open(str(path))
        try:
            stream = container.streams.video[0]
            for frame in container.decode(stream):
                idx = len(out)
                if idx >= expected_frames:
                    break
                if frame.pts is not None and frame.time_base is not None:
                    seconds = float(frame.pts * frame.time_base)
                    pts_ticks = tb.to_ticks(seconds)
                else:
                    pts_ticks = idx * frame_ticks
                out.append(
                    DecodedFrame(idx, pts_ticks, frame.to_ndarray(format="rgb24"))
                )
        finally:
            container.close()
    except DecodeError:
        raise
    except Exception as exc:  # PyAV 各类解码/容器错误统一归位
        raise DecodeError(
            f"解码失败 {path}: {exc}", first_position=len(out)
        ) from exc
    if len(out) < expected_frames:
        raise DecodeError(
            f"解码帧数不足: 期望 {expected_frames}，实得 {len(out)}",
            first_position=len(out),
        )
    return out


def composite_frame(timeline: Timeline, ticks: int, sources: dict[str, dict],
                    size: tuple[int, int]) -> tuple[np.ndarray, list[str]]:
    """按时间线合成一帧；返回 (图像, 各层来源描述)。"""
    width, height = size
    layers = timeline.composition_at(ticks)
    if not layers:
        return np.zeros((height, width, 3), dtype=np.uint8), ["gap"]
    acc = np.zeros((height, width, 3), dtype=np.float64)
    notes: list[str] = []
    for layer in layers:
        frames = sources.get(layer.source_id)
        key = layer.source_ticks
        if frames is not None and key in frames:
            img = frames[key].array.astype(np.float64)
            notes.append(f"{layer.source_id}@{key}")
        else:
            if timeline.fill_policy == FILL_ABORT:
                raise DecodeError(
                    f"缺帧且策略为中止: {layer.source_id}@{key}",
                    first_position=None,
                )
            img = np.zeros((height, width, 3), dtype=np.float64)
            notes.append(f"{layer.source_id}@{key}:filled")
        acc += img * layer.weight
    return np.clip(acc, 0, 255).astype(np.uint8), notes


def psnr_db(a: np.ndarray, b: np.ndarray) -> float:
    mse = float(np.mean((a.astype(np.float64) - b.astype(np.float64)) ** 2))
    if mse == 0.0:
        return 99.0
    return 10.0 * np.log10(255.0**2 / mse)


def pack_frames(frames: list[np.ndarray]) -> bytes:
    """把一帧序列打包为 npy 字节流（检查点落盘格式）。"""
    buf = io.BytesIO()
    np.save(buf, np.stack(frames), allow_pickle=False)
    return buf.getvalue()


def unpack_frames(payload: bytes) -> list[np.ndarray]:
    arr = np.load(io.BytesIO(payload), allow_pickle=False)
    return [arr[i] for i in range(arr.shape[0])]
