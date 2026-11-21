"""流水线内部使用的不可变包记录与压缩负载描述。

不复制压缩字节本身：``payload`` 持有对源端 bytes 的引用，跨队列传递；
校验和在解复用阶段逐包计算一次，回读阶段重新计算并比对。
"""
from __future__ import annotations

import hashlib
from dataclasses import dataclass
from fractions import Fraction
from typing import Any

from .timebase import TimeBaseLike, as_fraction, to_microseconds


@dataclass(frozen=True, slots=True)
class Payload:
    """压缩包负载：字节 + 源文件中的字节范围（半开区间）。"""

    data: bytes
    source_offset: int
    source_end: int

    @property
    def size(self) -> int:
        return len(self.data)

    def sha256(self) -> str:
        return hashlib.sha256(self.data).hexdigest()


@dataclass(frozen=True, slots=True)
class PacketRecord:
    """一个压缩包的完整元数据（不解码、不重编码）。"""

    stream_index: int
    sequence: int                     # 全局解复用序号（从 0 起，严格递增）
    pts: int | None
    dts: int | None
    time_base: Fraction
    pts_us: int | None
    dts_us: int | None
    duration: int | None              # 原始 time_base 刻度
    duration_us: int | None
    is_keyframe: bool
    is_corrupt: bool
    payload: Payload

    @property
    def size(self) -> int:
        return self.payload.size

    def to_timeline_dict(self) -> dict[str, Any]:
        return {
            "sequence": self.sequence,
            "stream_index": self.stream_index,
            "pts": self.pts,
            "dts": self.dts,
            "pts_us": self.pts_us,
            "dts_us": self.dts_us,
            "size": self.size,
            "is_keyframe": self.is_keyframe,
            "sha256": self.payload.sha256(),
        }


def build_record(
    packet: Any,
    sequence: int,
    *,
    source_offset: int,
    source_end: int,
    time_base_override: TimeBaseLike | None = None,
) -> PacketRecord:
    """从 PyAV packet 构建记录；time_base 缺省取 packet.time_base。"""
    tb = as_fraction(time_base_override or packet.time_base)
    data = bytes(packet)
    pts = None if packet.pts is None else int(packet.pts)
    dts = None if packet.dts is None else int(packet.dts)
    duration = None if packet.duration is None else int(packet.duration)
    return PacketRecord(
        stream_index=int(packet.stream_index),
        sequence=sequence,
        pts=pts,
        dts=dts,
        time_base=tb,
        pts_us=None if pts is None else to_microseconds(pts, tb),
        dts_us=None if dts is None else to_microseconds(dts, tb),
        duration=duration,
        duration_us=None if duration is None else to_microseconds(duration, tb),
        is_keyframe=bool(packet.is_keyframe),
        is_corrupt=bool(getattr(packet, "is_corrupt", False)),
        payload=Payload(data, source_offset, source_end),
    )
