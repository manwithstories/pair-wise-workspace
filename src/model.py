"""不可变领域模型：时间基、素材、片段、变速段、序列与统一交换文档。

所有模型均为 frozen dataclass，保证重复运行结果可复现、可安全共享。
时间量统一以“帧整数”表达，时间基与速率统一用 :class:`fractions.Fraction`
精确表达，避免浮点累计误差。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Mapping, Optional, Sequence, Tuple


@dataclass(frozen=True)
class TimeBase:
    """时间基，例如 24/1、30000/1001。"""

    rate: Fraction

    @classmethod
    def parse(cls, text: str) -> "TimeBase":
        text = text.strip()
        if "/" in text:
            num, den = text.split("/", 1)
            return cls(Fraction(int(num), int(den)))
        return cls(Fraction(int(text), 1))

    def __str__(self) -> str:
        if self.rate.denominator == 1:
            return f"{self.rate.numerator}/1"
        return f"{self.rate.numerator}/{self.rate.denominator}"


@dataclass(frozen=True)
class SpeedSegment:
    """片段内的一段变速区间（目标帧区间 + 速率，负速率表示反向）。"""

    dst_in: int
    dst_out: int
    rate: Fraction


@dataclass(frozen=True)
class Alias:
    """素材别名记录，可形成多跳别名链。"""

    alias: str
    target_id: str


@dataclass(frozen=True)
class Asset:
    """素材清单中的一条素材记录。"""

    asset_id: str
    rel_path: str
    size: int
    fingerprint: str                       # SHA-256 片段指纹（十六进制）
    fp_offset: int = 0
    fp_length: Optional[int] = None        # None 表示覆盖整个文件
    duration_frames: Optional[int] = None
    timebase: Optional[TimeBase] = None
    aliases: Tuple[Alias, ...] = ()
    extra: Mapping[str, str] = field(default_factory=dict)


@dataclass(frozen=True)
class Clip:
    """轨道上的片段记录：普通素材片段或嵌套序列引用，二选一。"""

    clip_id: str
    dst_in: int
    dst_out: int
    src_in: int
    src_out: int
    track_id: str
    asset_id: Optional[str] = None
    seq_ref: Optional[str] = None
    timebase: Optional[TimeBase] = None
    speed: Optional[Fraction] = None       # 单速率快捷写法；None 表示 1.0
    speed_segments: Tuple[SpeedSegment, ...] = ()
    extra: Mapping[str, str] = field(default_factory=dict)


@dataclass(frozen=True)
class Track:
    track_id: str
    kind: str = "video"
    clips: Tuple[Clip, ...] = ()
    extra: Mapping[str, str] = field(default_factory=dict)


@dataclass(frozen=True)
class Sequence:
    sequence_id: str
    timebase: TimeBase
    tracks: Tuple[Track, ...] = ()
    extra: Mapping[str, str] = field(default_factory=dict)


@dataclass(frozen=True)
class ExchangeDoc:
    """两条导入路径汇聚后的统一模型。"""

    version: str
    timebase: TimeBase
    assets: Tuple[Asset, ...] = ()
    sequences: Tuple[Sequence, ...] = ()
    extra: Mapping[str, str] = field(default_factory=dict)

    def asset_map(self) -> Mapping[str, Asset]:
        return {a.asset_id: a for a in self.assets}

    def sequence_map(self) -> Mapping[str, Sequence]:
        return {s.sequence_id: s for s in self.sequences}

    def root_sequences(self) -> Sequence[Sequence]:
        """顶层序列：未被任何片段引用者。

        若所有序列都被引用（纯循环图），退化为声明顺序的第一个序列，
        保证循环引用仍有遍历入口、可被时间线展开检测并剪枝。
        """
        referenced = {
            c.seq_ref
            for seq in self.sequences
            for tr in seq.tracks
            for c in tr.clips
            if c.seq_ref is not None
        }
        roots = tuple(s for s in self.sequences if s.sequence_id not in referenced)
        if roots:
            return roots
        return self.sequences[:1]


@dataclass(frozen=True)
class LeafClip:
    """嵌套展开后的叶子片段（必然指向素材），保留完整父子来源链。"""

    entry_id: int
    asset_id: str
    track_id: str
    dst_in: int                            # 顶层序列时间轴帧
    dst_out: int
    src_in: int
    src_out: int
    rate: Fraction
    sequence_chain: Tuple[str, ...]        # 从顶层到直接父序列
    clip_chain: Tuple[str, ...]            # 各层片段 id
    depth: int
    timebase: TimeBase
    drift_frames: int = 0                  # 多段变速累计取整误差（帧）


@dataclass(frozen=True)
class Issue:
    severity: str                          # "error" | "warning"
    code: str
    message: str
    entry_id: Optional[int] = None
    asset_id: Optional[str] = None
    sequence_id: Optional[str] = None


@dataclass(frozen=True)
class ConformEntry:
    entry_id: int
    asset_id: str
    src_in: int
    src_out: int
    target_duration: int
    track_id: str
    sequence_chain: Tuple[str, ...]
    locate_status: str                     # located | missing | ambiguous
    drift_frames: int = 0
    candidates: Tuple[str, ...] = ()
    issues: Tuple[Issue, ...] = ()


# 定位状态常量
LOCATED = "located"
MISSING = "missing"
AMBIGUOUS = "ambiguous"

# 问题严重度
ERROR = "error"
WARNING = "warning"

# 指纹算法约定（fixture 与定位共用）
FP_ALGO = "sha256-partial"
FP_CHUNK = 64 * 1024
