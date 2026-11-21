"""夹具生成与损坏注入（纯本地，不引用外部文件）。

* :func:`generate_sample` 生成带时间戳的多轨 MP4/MOV（H.264 + AAC），
  音频 PCM 由 NumPy 生成确定性波形；
* 一组 ``corrupt_*`` 函数在字节层注入：尾部截断、坏 box、坏包、缺失索引，
  并返回已知的真实字节偏移，供测试断言诊断定位是否准确。

所有损坏函数都尽量给出“首个坏字节”的精确位置；只能确定范围时返回
半开区间，调用方据此构造 :class:`SourceOffset`，绝不伪称精确定位。
"""
from __future__ import annotations

import os
from dataclasses import dataclass

import av
import numpy as np

# MP4 box 头：4 字节 size + 4 字节 type
_BOX_HEADER = 8


@dataclass(slots=True)
class CorruptionSpec:
    kind: str
    exact_offset: int | None
    offset_start: int
    offset_end: int
    note: str


def _add_av_tracks(container: av.container.OutputContainer, frames: int) -> None:
    vs = container.add_stream("h264", rate=24)
    vs.width = 128
    vs.height = 96
    vs.pix_fmt = "yuv420p"
    aus = container.add_stream("aac", rate=48000)
    aus.layout = "stereo"
    aus.format = "fltp"
    rng = np.random.default_rng(42)
    for i in range(frames):
        # 确定性视频内容（渐变 + 噪声），保证关键帧差异
        img = np.full((96, 128, 3), (i * 4) % 256, dtype=np.uint8)
        img[..., 1] = rng.integers(0, 255, size=(96, 128), dtype=np.uint8)
        for pkt in vs.encode(av.VideoFrame.from_ndarray(img, format="rgb24")):
            container.mux(pkt)
        # 确定性正弦 PCM（双声道），编码立即 mux，避免交错时序问题
        t = np.arange(1024) / 48000.0
        wave = (0.2 * np.sin(2 * np.pi * 440 * t)).astype(np.float32)
        pcm = np.ascontiguousarray(np.vstack([wave, wave]))
        frame = av.AudioFrame.from_ndarray(pcm, format="fltp", layout="stereo")
        frame.sample_rate = 48000
        frame.pts = i * 1024
        for pkt in aus.encode(frame):
            container.mux(pkt)
    for pkt in vs.encode():
        container.mux(pkt)
    for pkt in aus.encode():
        container.mux(pkt)


def generate_sample(path: str, frames: int = 300, fragmented: bool = False) -> str:
    """生成约百万字节级别的双轨样例；返回文件路径。"""
    options = (
        {"movflags": "+frag_keyframe+empty_moov+default_base_moof"}
        if fragmented
        else None
    )
    with av.open(path, mode="w", options=options) as container:
        _add_av_tracks(container, frames)
    return path


def _read(path: str) -> bytearray:
    with open(path, "rb") as fh:
        return bytearray(fh.read())


def _write(path: str, data: bytearray) -> None:
    with open(path, "wb") as fh:
        fh.write(data)
        fh.flush()
        os.fsync(fh.fileno())


def _find_boxes(data: bytearray) -> list[tuple[bytes, int, int]]:
    """扫描顶层 box，返回 (type, start, end) 列表。"""
    boxes: list[tuple[bytes, int, int]] = []
    pos = 0
    while pos + _BOX_HEADER <= len(data):
        size = int.from_bytes(data[pos:pos + 4], "big")
        btype = bytes(data[pos + 4:pos + 8])
        if size < _BOX_HEADER or pos + size > len(data):
            break
        boxes.append((btype, pos, pos + size))
        pos += size
    return boxes


def corrupt_truncated(src: str, dst: str, keep_ratio: float = 0.7) -> CorruptionSpec:
    """尾部截断：首个坏字节 = 截断点（精确）。"""
    data = _read(src)
    cut = int(len(data) * keep_ratio)
    _write(dst, data[:cut])
    return CorruptionSpec(
        kind="truncated",
        exact_offset=cut,
        offset_start=cut,
        offset_end=len(data),
        note="尾部截断，首个缺失字节即截断点",
    )


def corrupt_box(src: str, dst: str, box_type: bytes = b"moov") -> CorruptionSpec:
    """损坏指定 box 的类型字段；box 起点精确，坏字节为 type 四字节区间。"""
    data = _read(src)
    for btype, start, end in _find_boxes(data):
        if btype == box_type:
            data[start + 4:start + 8] = b"zzzz"
            _write(dst, data)
            return CorruptionSpec(
                kind="corrupt_box",
                exact_offset=None,
                offset_start=start + 4,
                offset_end=start + 8,
                note=f"{box_type.decode()} box 类型字段被覆写",
            )
    # 找不到目标 box：无法精确归因，给出文件级区间
    _write(dst, data)
    return CorruptionSpec(
        kind="corrupt_box",
        exact_offset=None,
        offset_start=0,
        offset_end=len(data),
        note=f"未找到 {box_type!r} box，无法精确定位",
    )


def corrupt_packet(src: str, dst: str, *, target_ratio: float = 0.5) -> CorruptionSpec:
    """篡改 stsz 样本大小表，使容器按错误长度读取压缩包。

    MP4 的每包长度记录在 moov/stsz，篡改后解包会越界或错位，PyAV
    在 demux 阶段报错。stsz 入口在 moov 内可精确定位，给出该入口的
    4 字节区间；具体哪个数据包受影响由运行时解析界定，不伪称包级精确。
    """
    data = _read(src)
    boxes = dict((t, (s, e)) for t, s, e in _find_boxes(data))
    if b"mdat" not in boxes:
        _write(dst, data)
        return CorruptionSpec(
            kind="corrupt_packet",
            exact_offset=None,
            offset_start=0,
            offset_end=len(data),
            note="无 mdat box，无法定位损坏包",
        )
    stsz = _find_child_box(data, b"stsz")
    if stsz is None:
        # 回退：mdat 中段翻转（可能不致命，仅作保守区间）
        mdat_start, mdat_end = boxes[b"mdat"]
        payload_start = mdat_start + _BOX_HEADER
        target = int(payload_start + (mdat_end - payload_start) * target_ratio)
        data[target] ^= 0xFF
        _write(dst, data)
        return CorruptionSpec(
            kind="corrupt_packet",
            exact_offset=None,
            offset_start=payload_start,
            offset_end=target + 1,
            note="无 stsz，回退为 mdat 字节翻转",
        )
    _, s_start, _ = stsz
    # stsz: 4 size/4 type/4 ver+flags/4 uniform_size/4 sample_count(偏移+16)
    # 膨胀声明样本数，使容器按不存在的样本取包，解包流提前枯竭
    count_off = s_start + 16
    original_count = int.from_bytes(data[count_off:count_off + 4], "big")
    data[count_off:count_off + 4] = (original_count + 50_000).to_bytes(4, "big")
    _write(dst, data)
    return CorruptionSpec(
        kind="corrupt_packet",
        exact_offset=None,
        offset_start=count_off,
        offset_end=count_off + 4,
        note="stsz 样本计数被膨胀，解包流将静默截断",
    )


def _find_child_box(data: bytearray, wanted: bytes, *, start: int = 0, end: int | None = None):
    """在容器区间内递归查找指定类型 box，返回 (type, start, end) 或 None。"""
    end = len(data) if end is None else end
    pos = start
    while pos + _BOX_HEADER <= end:
        size = int.from_bytes(data[pos:pos + 4], "big")
        btype = bytes(data[pos + 4:pos + 8])
        if size < _BOX_HEADER or pos + size > end:
            break
        if btype == wanted:
            return (btype, pos, pos + size)
        # 容器类 box 递归
        if btype in (b"moov", b"trak", b"mdia", b"minf", b"stbl"):
            found = _find_child_box(
                data, wanted, start=pos + _BOX_HEADER, end=pos + size
            )
            if found is not None:
                return found
        pos += size
    return None


def corrupt_missing_index(src: str, dst: str) -> CorruptionSpec:
    """删除 moov box（容器索引/元数据），模拟缺失关键索引。"""
    data = _read(src)
    for btype, start, end in _find_boxes(data):
        if btype == b"moov":
            removed = data[start:end]
            del data[start:end]
            _write(dst, data)
            return CorruptionSpec(
                kind="missing_index",
                exact_offset=start,
                offset_start=start,
                offset_end=end,
                note="moov 索引 box 被移除",
            )
    _write(dst, data)
    return CorruptionSpec(
        kind="missing_index",
        exact_offset=None,
        offset_start=0,
        offset_end=len(data),
        note="未找到 moov box",
    )
