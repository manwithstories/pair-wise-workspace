"""演示与测试素材的本地生成器（PyAV 编码 + soundfile 写 WAV）。

全部素材在运行目录内自生成，不读取外部来源、不联网。
视频输出 Annex B（h264/h265）或长度前缀 OBU（av1）裸流，
音频输出带已知静音区间的 16-bit PCM WAV，供静音检测核对。
"""

from __future__ import annotations

import os
from dataclasses import dataclass

import av
import numpy as np
import soundfile as sf

from .bitstream import CODECS

_ENCODER = {"h264": "libx264", "h265": "libx265", "av1": "libaom-av1"}
_AUDIO_RATE = 48_000


@dataclass(slots=True)
class GeneratedVideo:
    path: str
    codec: str
    frame_count: int
    fps: int
    width: int
    height: int
    gop: int
    sha256: str


@dataclass(slots=True)
class GeneratedAudio:
    path: str
    sample_rate: int
    channels: int
    duration_us: int
    # 已知静音区间（整数微秒，闭开区间），测试用 ground truth
    silence_ranges_us: list[tuple[int, int]]


def _synthetic_frame(index: int, width: int, height: int) -> np.ndarray:
    """确定性合成画面：移动色块 + 行渐变，保证不同帧内容不同但可复现。"""

    img = np.zeros((height, width, 3), dtype=np.uint8)
    ramp = np.linspace(0, 255, width, dtype=np.uint8)
    img[:, :, 1] = ramp[None, :]
    img[:, :, 2] = ramp[::-1][None, :]
    band = (index * 8) % max(width - 16, 1)
    img[8: height - 8, band: band + 16, 0] = 200 + (index * 7) % 56
    return img


def _leb128(value: int) -> bytes:
    out = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            out.append(byte | 0x80)
        else:
            out.append(byte)
            return bytes(out)


def _ivf_to_low_overhead(ivf_path: str, out_path: str) -> None:
    """IVF 解封装为带 obu_size 的 low-overhead 裸 OBU 流。

    libaom 经 IVF 输出的每帧结构为：temporal delimiter（固定两字节
    0x12 0x00，空载荷）后跟本帧其余 OBU 的拼接，且这些 OBU 头不含
    obu_size 字段。IVF 帧头给出整帧字节数，因此 TD 之后到帧尾恰好是
    一个连续的 OBU 段——把该段逐 OBU 头切出并补写 LEB128 长度即可，
    边界完全由容器确定，不依赖内容启发式。
    """

    with open(ivf_path, "rb") as fh:
        blob = fh.read()
    if blob[:4] != b"DKIF":
        raise ValueError("非法 IVF 文件头")
    pos = 32
    pieces: list[bytes] = []
    while pos + 12 <= len(blob):
        frame_len = int.from_bytes(blob[pos:pos + 4], "little")
        frame = blob[pos + 12: pos + 12 + frame_len]
        pos += 12 + frame_len
        cur = 0
        # 跳过所有前导 temporal delimiter
        while cur + 1 < len(frame) and frame[cur:cur + 2] == b"\x12\x00":
            cur += 2
        while cur < len(frame):
            header = frame[cur]
            if header & 0x80:
                raise ValueError("AV1 OBU forbidden 位置位")
            obu_type = (header >> 3) & 0x0F
            header_size = 2 + (1 if header & 0x04 else 0)
            if cur + header_size > len(frame):
                raise ValueError("AV1 OBU 头跨越 IVF 帧边界")
            if obu_type == 2:  # 残余 TD（理论上前导跳过后不应出现）
                cur += 2
                continue
            # 帧内最后一个 OBU：长度由 IVF 帧边界确定
            payload = frame[cur + header_size:]
            pieces.append(
                bytes([header | 0x02]) + _leb128(len(payload)) + payload)
            break
    with open(out_path, "wb") as fh:
        fh.write(b"".join(pieces))


def generate_video(
    path: str,
    codec: str,
    *,
    duration_s: float = 12.0,
    fps: int = 30,
    width: int = 160,
    height: int = 90,
    gop: int = 15,
    seed: int = 12789,
) -> GeneratedVideo:
    if codec not in CODECS:
        raise ValueError(f"不支持的编码标识: {codec!r}")
    np.random.seed(seed)
    frame_count = max(1, round(duration_s * fps))
    mux_format = "ivf" if codec == "av1" else "data"
    mux_path = path + ".ivf" if codec == "av1" else path
    container = av.open(mux_path, "w", format=mux_format)
    try:
        stream = container.add_stream(_ENCODER[codec], rate=fps)
        stream.width, stream.height = width, height
        stream.pix_fmt = "yuv420p"
        stream.options = {"g": str(gop), "keyint_min": str(gop)}
        if codec == "av1":
            stream.options["usage"] = "realtime"
            stream.options["cpu-used"] = "8"
        for idx in range(frame_count):
            frame = av.VideoFrame.from_ndarray(
                _synthetic_frame(idx, width, height), format="rgb24")
            frame.pts = idx
            for packet in stream.encode(frame):
                container.mux(packet)
        for packet in stream.encode(None):
            container.mux(packet)
    finally:
        container.close()
    if codec == "av1":
        _ivf_to_low_overhead(mux_path, path)
        os.remove(mux_path)
    from .timeline import sha256_hex
    digest = sha256_hex(open(path, "rb").read())
    return GeneratedVideo(path, codec, frame_count, fps, width, height, gop,
                          digest)


def generate_audio(
    path: str,
    *,
    duration_s: float = 12.0,
    sample_rate: int = _AUDIO_RATE,
    channels: int = 2,
    silence_ranges_s: tuple[tuple[float, float], ...] = (
        (2.0, 3.0), (6.5, 7.25), (10.0, 12.0)),
    tone_hz: float = 440.0,
    seed: int = 12789,
) -> GeneratedAudio:
    """生成带已知静音段的 WAV；尾截断场景由调用方直接裁剪文件。"""

    rng = np.random.default_rng(seed)
    total = round(duration_s * sample_rate)
    t = np.arange(total) / sample_rate
    tone = 0.25 * np.sin(2 * np.pi * tone_hz * t)
    tone += 0.05 * rng.standard_normal(total)
    pcm = np.empty((total, channels), dtype=np.float32)
    silence_mask = np.zeros(total, dtype=bool)
    ranges_us: list[tuple[int, int]] = []
    for start_s, end_s in silence_ranges_s:
        s, e = round(start_s * sample_rate), round(end_s * sample_rate)
        e = min(e, total)
        if s >= e:
            continue
        silence_mask[s:e] = True
        ranges_us.append((round(start_s * 1_000_000),
                          round((e / sample_rate) * 1_000_000)))
    tone[silence_mask] = 0.0
    for ch in range(channels):
        pcm[:, ch] = tone
    sf.write(path, pcm, sample_rate, subtype="PCM_16")
    from .timeline import seconds_to_us
    return GeneratedAudio(
        path, sample_rate, channels, seconds_to_us(total / sample_rate),
        ranges_us)


def truncate_file(path: str, at_byte: int) -> int:
    """在指定字节处截断文件，返回实际长度。"""

    size = os.path.getsize(path)
    target = max(0, min(at_byte, size))
    with open(path, "r+b") as fh:
        fh.truncate(target)
    return target


def write_index(path: str, codec: str, entries: list[int]) -> None:
    """供缺页夹具使用：写一个可能不完整的索引 JSON。"""

    import json
    payload = {
        "codec": codec,
        "incomplete": False,
        "entries": [{"offset": off, "pts_us": None,
                     "source": "index", "repaired": False}
                    for off in entries],
        "errors": [],
    }
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, ensure_ascii=False, indent=2, sort_keys=True)
