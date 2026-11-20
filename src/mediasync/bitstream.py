"""H.264 / H.265 / AV1 裸码流（Annex B / low-overhead）扫描。

职责：
1. 定位 Annex B 起始码，输出每个 NAL/OBU 的字节偏移与类型；
2. 识别三类编码的关键帧（IDR/CRA/keyframe）；
3. 校验外部关键帧索引：拒绝越界、倒序、重复项；
4. 报告截断与损坏（无起始码、禁止位、空单元）。

扫描器只做字节级解析，不依赖解码器，保证纯本地、确定性。
"""

from __future__ import annotations

import dataclasses
import enum
from fractions import Fraction

from .clock import MediaClock, TimelineBuilder
from .errors import ErrorBook, ErrorCode

START_CODE3 = b"\x00\x00\x01"
START_CODE4 = b"\x00\x00\x00\x01"


class Codec(str, enum.Enum):
    H264 = "h264"
    H265 = "h265"
    AV1 = "av1"


@dataclasses.dataclass(frozen=True)
class NalUnit:
    """一个 NAL/OBU 单元的扫描结果。"""

    offset: int
    """起始码第一个字节在文件中的偏移。"""
    nal_type: int
    size: int
    """含起始码的总字节数。"""
    is_keyframe: bool
    codec: str


@dataclasses.dataclass(frozen=True)
class KeyframeEntry:
    """外部关键帧索引中的一项。"""

    offset: int
    tick: int


def _find_start_codes(data: bytes) -> list[tuple[int, int]]:
    """返回 [(起始码偏移, 起始码长度)]，起始码长度为 3 或 4。"""
    out: list[tuple[int, int]] = []
    n = len(data)
    i = 0
    while i < n - 2:
        if data[i] == 0 and data[i + 1] == 0:
            if data[i + 2] == 1:
                out.append((i, 3))
                i += 3
                continue
            if i + 3 < n and data[i + 2] == 0 and data[i + 3] == 1:
                out.append((i, 4))
                i += 4
                continue
        i += 1
    return out


def _h264_is_key(nal_type: int) -> bool:
    # 5=IDR；SPS/PPS 为参数集，不是帧。
    return nal_type == 5


def _h265_is_key(nal_type: int) -> bool:
    # 19/20=IDR_W_RADL/IDR_N_LP，21=CRA。
    return nal_type in (19, 20, 21)


def _av1_is_key(obu_type: int, body: bytes) -> bool:
    # OBU_FRAME=6 / OBU_FRAME_HEADER=3；frame_type 2 bit，KEY_FRAME=0。
    if obu_type not in (3, 6) or not body:
        return False
    return (body[0] & 0xC0) == 0x00


def scan_bitstream(
    data: bytes,
    codec: Codec | str,
    *,
    source: str = "",
    errors: ErrorBook | None = None,
) -> list[NalUnit]:
    """扫描裸码流，返回 NAL/OBU 序列并向 ``errors`` 记录问题。"""
    codec = Codec(codec)
    book = errors if errors is not None else ErrorBook()
    marks = _find_start_codes(data)
    if not marks:
        book.add(
            ErrorCode.BITSTREAM_CORRUPT,
            f"{source}: 未找到任何起始码，不是合法 Annex B 裸码流",
            source=source,
            fatal=True,
        )
        return []

    units: list[NalUnit] = []
    for idx, (offset, sc_len) in enumerate(marks):
        end = marks[idx + 1][0] if idx + 1 < len(marks) else len(data)
        payload = data[offset + sc_len : end]
        if not payload:
            book.add(
                ErrorCode.BITSTREAM_TRUNCATED,
                f"{source}: 偏移 {offset} 处单元为空",
                source=source,
                offset=offset,
                fatal=True,
            )
            continue
        if codec is Codec.AV1:
            header = payload[0]
            obu_type = (header >> 3) & 0x0F
            # low-overhead 格式无 obu_size 字段，body 即剩余字节。
            key = _av1_is_key(obu_type, payload[1:])
            nal_type = obu_type
        else:
            nal_type = (
                payload[0] & 0x1F if codec is Codec.H264 else (payload[0] >> 1) & 0x3F
            )
            if payload[0] & 0x80:
                book.add(
                    ErrorCode.BITSTREAM_CORRUPT,
                    f"{source}: 偏移 {offset} 处 NAL 禁止位为 1",
                    source=source,
                    offset=offset,
                    fatal=True,
                )
            key = (
                _h264_is_key(nal_type)
                if codec is Codec.H264
                else _h265_is_key(nal_type)
            )
        units.append(
            NalUnit(
                offset=offset,
                nal_type=nal_type,
                size=end - offset,
                is_keyframe=key,
                codec=codec.value,
            )
        )
    return units


def validate_keyframe_index(
    entries: list[KeyframeEntry],
    units: list[NalUnit],
    file_size: int,
    *,
    source: str = "",
    errors: ErrorBook | None = None,
) -> list[KeyframeEntry]:
    """校验外部关键帧索引：越界 / 重复 / 倒序 / 未指向关键帧。"""
    book = errors if errors is not None else ErrorBook()
    valid_offsets = {u.offset for u in units if u.is_keyframe}
    seen: set[int] = set()
    previous = -1
    accepted: list[KeyframeEntry] = []
    for entry in entries:
        if entry.offset < 0 or entry.offset >= file_size:
            book.add(
                ErrorCode.INDEX_OUT_OF_RANGE,
                f"{source}: 索引偏移 {entry.offset} 越界（文件 {file_size} 字节）",
                source=source,
                offset=entry.offset,
                fatal=True,
            )
            continue
        if entry.offset in seen:
            book.add(
                ErrorCode.INDEX_DUPLICATE,
                f"{source}: 重复索引项 offset={entry.offset}",
                source=source,
                offset=entry.offset,
                fatal=True,
            )
            continue
        if entry.offset <= previous:
            book.add(
                ErrorCode.INDEX_REVERSED,
                f"{source}: 索引偏移倒序 {previous} -> {entry.offset}",
                source=source,
                offset=entry.offset,
                fatal=True,
            )
            continue
        if valid_offsets and entry.offset not in valid_offsets:
            book.add(
                ErrorCode.BITSTREAM_CORRUPT,
                f"{source}: 索引偏移 {entry.offset} 未指向扫描到的关键帧",
                source=source,
                offset=entry.offset,
                fatal=True,
            )
            continue
        seen.add(entry.offset)
        previous = entry.offset
        accepted.append(entry)
    return accepted


def build_timeline(
    entries: list[KeyframeEntry],
    clock: MediaClock,
    *,
    source: str,
    errors: ErrorBook,
) -> TimelineBuilder:
    """把关键帧索引的 tick 喂入共享时钟，得到分段连续时间线。"""
    builder = TimelineBuilder(clock, source, errors)
    for entry in entries:
        builder.add(entry.tick, kind="keyframe")
    return builder


def keyframe_times_ms(entries: list[KeyframeEntry], clock: MediaClock) -> list[Fraction]:
    """索引单调场景下的关键帧连续毫秒时间（有理数）。"""
    return [clock.tick_to_ms(e.tick) for e in entries]
