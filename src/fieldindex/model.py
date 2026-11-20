"""统一模型层：有理数时间基准、错误记录、索引条目与安全路径。

所有时间戳一律以 ``Fraction`` 表示的“媒体时间线秒数”承载，
避免 29.97 等分数帧率在浮点换算中产生不可逆误差。
"""
from __future__ import annotations

import os
from dataclasses import dataclass, field, asdict
from fractions import Fraction
from pathlib import Path

#: 单条字幕文本最大字符数（超限隔离该条并保留偏移证据）。
MAX_CUE_CHARS = 8_000
#: 单路输入字节流上限（防止坏数据耗尽内存）。
MAX_INPUT_BYTES = 64 * 1024 * 1024


class FieldIndexError(Exception):
    """输入级错误：携带出错时的字节偏移，便于追溯原始字节。"""

    def __init__(self, message: str, offset: int = -1) -> None:
        super().__init__(message)
        self.offset = offset


@dataclass(frozen=True)
class SourceRef:
    """原始字节定位：输入标识 + 半开区间 [start, end) 字节偏移。"""

    source: str
    start: int
    end: int

    def as_dict(self) -> dict:
        return {"source": self.source, "start": self.start, "end": self.end}


@dataclass
class ErrorRecord:
    """分阶段错误记录，贯穿解析、修复、索引与回读各阶段。

    action 描述恢复动作（truncated-skip / wrap / resequence /
    drift-clamp / conflict-mark / quarantine / rebuild ...），
    recoverable=False 的数据被隔离，后续可读数据继续处理。
    """

    source: str
    start: int
    end: int
    stage: str
    reason: str
    action: str = "quarantine"
    recoverable: bool = False

    def as_dict(self) -> dict:
        return asdict(self)


@dataclass
class Cue:
    """字幕 cue；start/end 为统一时间基准上的秒（Fraction）。"""

    seq: int
    start: Fraction
    end: Fraction
    text: str
    ref: SourceRef
    flags: list[str] = field(default_factory=list)

    def as_dict(self) -> dict:
        return {
            "seq": self.seq,
            "start": str(self.start),
            "end": str(self.end),
            "text": self.text,
            "ref": self.ref.as_dict(),
            "flags": list(self.flags),
        }


@dataclass
class TimecodeRecord:
    """SMPTE 时间码记录；frame 为 24h 周期内的帧序号。"""

    frame: int
    rate_fps: Fraction
    drop_frame: bool
    raw: str
    ref: SourceRef
    timeline: Fraction | None = None
    flags: list[str] = field(default_factory=list)

    def as_dict(self) -> dict:
        return {
            "frame": self.frame,
            "rate_fps": str(self.rate_fps),
            "drop_frame": self.drop_frame,
            "raw": self.raw,
            "ref": self.ref.as_dict(),
            "timeline": None if self.timeline is None else str(self.timeline),
            "flags": list(self.flags),
        }


@dataclass
class Keyframe:
    """媒体关键帧定位（时间基准秒 + 帧序号），来自 PyAV 流探测。"""

    index: int
    pts_time: Fraction
    is_key: bool


@dataclass
class MediaInfo:
    """媒体时间基准与关键帧信息。"""

    source: str
    frame_rate: Fraction
    duration: Fraction
    keyframes: list[Keyframe] = field(default_factory=list)


@dataclass
class IndexEntry:
    """可随机访问的索引条目，指向修复后时间线上的一条记录。"""

    segment_id: int
    kind: str  # "cue" | "timecode"
    seq: int
    start: Fraction
    end: Fraction
    ref: SourceRef
    flags: list[str] = field(default_factory=list)

    def as_dict(self) -> dict:
        return {
            "segment_id": self.segment_id,
            "kind": self.kind,
            "seq": self.seq,
            "start": str(self.start),
            "end": str(self.end),
            "ref": self.ref.as_dict(),
            "flags": list(self.flags),
        }


@dataclass
class Segment:
    """分段记录：边界、条目数与覆盖字节区间的 SHA-256 证据。"""

    segment_id: int
    start: Fraction
    end: Fraction
    entry_count: int
    byte_start: int
    byte_end: int
    sha256: str
    verified: bool = False

    def as_dict(self) -> dict:
        d = asdict(self)
        d["start"] = str(self.start)
        d["end"] = str(self.end)
        return d


def resolve_within(base_dir: str | os.PathLike[str], name: str) -> Path:
    """把相对路径限制在指定工作目录内，拒绝绝对路径、目录逃逸与符号链接。"""

    base = Path(base_dir).resolve()
    raw = Path(name)
    if raw.is_absolute():
        raise FieldIndexError(f"拒绝绝对路径: {name!r}")
    candidate_raw = (base / name).absolute()
    # 逐级检查，中间任一环节是符号链接都拒绝
    probe = base
    for part in raw.parts:
        probe = probe / part
        if probe.is_symlink():
            raise FieldIndexError(f"拒绝符号链接: {name!r}")
    candidate = candidate_raw.resolve()
    if base != candidate and base not in candidate.parents:
        raise FieldIndexError(f"路径越出工作目录: {name!r}")
    return candidate
