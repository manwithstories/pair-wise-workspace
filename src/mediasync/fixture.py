"""自生成媒体夹具：正常、截断、索引偏移、时间戳回退、元数据冲突。

素材全部由代码生成，不引用外部文件、不联网。视频用 PyAV 真实编码
（libx264 / libx265 / libaom-av1），再统一包装为 Annex B 起始码裸码流；
关键帧索引由编码结果扫描得到，保证偏移与时间戳可核对。
"""

from __future__ import annotations

import dataclasses
from pathlib import Path

import numpy as np
import av

from .bitstream import Codec, KeyframeEntry, _av1_leb128_size, scan_bitstream
from .errors import ErrorBook, ErrorCode
from .safety import resolve_within

CODEC_ENCODER = {
    Codec.H264: ("libx264", "h264"),
    Codec.H265: ("libx265", "hevc"),
    Codec.AV1: ("libaom-av1", "obu"),
}


@dataclasses.dataclass
class RepresentationSpec:
    name: str
    codec: Codec
    width: int = 128
    height: int = 72
    fps: int = 30
    gop: int = 15
    frames: int = 30
    timescale: int = 30


def _render_frame(index: int, width: int, height: int) -> av.VideoFrame:
    value = (index * 7) % 255
    arr = np.full((height, width, 3), value, dtype=np.uint8)
    frame = av.VideoFrame.from_ndarray(arr, format="rgb24")
    frame.pts = index
    return frame


def encode_video(spec: RepresentationSpec) -> bytes:
    """编码并返回 Annex B 裸码流字节（AV1 的 obu 输出会重包装起始码）。"""
    codec = Codec(spec.codec)
    encoder, mux_format = CODEC_ENCODER[codec]
    import io

    buffer = io.BytesIO()
    container = av.open(buffer, "w", format=mux_format)
    stream = container.add_stream(encoder, rate=spec.fps)
    stream.width, stream.height = spec.width, spec.height
    stream.options = {"g": str(spec.gop), "keyint_min": str(spec.gop), "crf": "30"}
    if codec is Codec.AV1:
        stream.options = {
            "usage": "realtime",
            "cpu-used": "8",
            "g": str(spec.gop),
            "keyint_min": str(spec.gop),
        }
    for index in range(spec.frames):
        for packet in stream.encode(_render_frame(index, spec.width, spec.height)):
            container.mux(packet)
    for packet in stream.encode(None):
        container.mux(packet)
    container.close()
    raw = buffer.getvalue()
    return wrap_annexb(raw, codec)


def wrap_annexb(raw: bytes, codec: Codec | str) -> bytes:
    """H.264/H.265 已是 Annex B；AV1 low-overhead 按 OBU 切分后加起始码。"""
    codec = Codec(codec)
    if codec is not Codec.AV1:
        return raw
    out = bytearray()
    i = 0
    while i < len(raw):
        header = raw[i]
        has_size = bool(header & 0x02)
        i += 1
        if not has_size:
            continue
        n = _av1_leb128_size(raw[i:])
        size, shift = 0, 0
        for byte in raw[i : i + n]:
            size |= (byte & 0x7F) << shift
            shift += 7
        obu = raw[i - 1 : i + n + size]
        out += b"\x00\x00\x01"
        out += obu
        i += n + size
    return bytes(out)


def build_keyframe_index(
    data: bytes, codec: Codec | str, timescale: int, fps: int, gop: int
) -> list[KeyframeEntry]:
    """扫描编码结果，关键帧 tick 按真实帧序号 * timescale/fps 生成。

    固定 GOP 下第 k 个关键帧对应帧号 k*gop，tick = 帧号 * timescale/fps。
    """
    book = ErrorBook()
    units = scan_bitstream(data, codec, source="fixture", errors=book)
    key_offsets = [u.offset for u in units if u.is_keyframe]
    tick_step = timescale // fps
    return [
        KeyframeEntry(offset=offset, tick=ordinal * gop * tick_step)
        for ordinal, offset in enumerate(key_offsets)
    ]


# ------------------------------------------------------------------
# 损坏注入：全部返回确定的损坏字节偏移，供验收核对
# ------------------------------------------------------------------

def inject_corrupt_header(data: bytes, nth: int) -> tuple[bytes, int]:
    """翻转第 nth 个 NAL/OBU 头部禁止位，返回 (新数据, 损坏偏移)。"""
    from .bitstream import _find_start_codes

    marks = _find_start_codes(data)
    target = marks[(nth - 1) % len(marks)]
    offset, sc_len = target
    header_offset = offset + sc_len
    mutated = bytearray(data)
    mutated[header_offset] |= 0x80
    return bytes(mutated), header_offset


def inject_truncated(data: bytes) -> tuple[bytes, int]:
    """在起始码边界截断，丢弃最后一个完整单元，返回截断偏移。"""
    from .bitstream import _find_start_codes

    marks = _find_start_codes(data)
    cut = marks[-1][0]
    return data[:cut], cut


def inject_index_offset(entries: list[KeyframeEntry], shift: int) -> list[KeyframeEntry]:
    """索引偏移错位：把第二项偏移加 shift，制造未指向关键帧。"""
    if len(entries) < 2:
        return entries
    broken = list(entries)
    target = broken[1]
    broken[1] = KeyframeEntry(offset=target.offset + shift, tick=target.tick)
    return broken


def inject_timestamp_backtrack(entries: list[KeyframeEntry]) -> list[KeyframeEntry]:
    """交换后两项的 tick，制造时间戳回退（偏移仍严格递增）。"""
    if len(entries) < 3:
        return entries
    broken = list(entries)
    a, b = broken[-2], broken[-1]
    broken[-2] = KeyframeEntry(offset=a.offset, tick=b.tick)
    broken[-1] = KeyframeEntry(offset=b.offset, tick=a.tick)
    return broken


def write_bytes(path: str | Path, root: str | Path, data: bytes) -> Path:
    safe = resolve_within(root, path)
    safe.parent.mkdir(parents=True, exist_ok=True)
    safe.write_bytes(data)
    return safe
