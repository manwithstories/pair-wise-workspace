"""分片传输恢复：按序号、URI 与内容摘要处理重复、乱序、迟到及缺失分片。"""
from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from pathlib import Path

from .errors import MediaError
from .manifest import Manifest, SegmentRef


@dataclass(frozen=True)
class Delivery:
    """一个已确认的分片投递（内容已读取并计算摘要）。"""

    ref: SegmentRef
    path: Path
    digest: str
    nbytes: int


@dataclass(frozen=True)
class Gap:
    """缺失/被隔离的媒体时间区间，时长取自清单声明，保证缺口时长准确。"""

    start: float
    duration: float
    reason: str  # "missing" | "conflict" | "decode"
    recoverable: bool = True

    @property
    def end(self) -> float:
        return self.start + self.duration


@dataclass
class TransportResult:
    deliveries: list[Delivery] = field(default_factory=list)  # 按媒体时间排序
    gaps: list[Gap] = field(default_factory=list)
    errors: list[MediaError] = field(default_factory=list)
    duplicates: int = 0       # 内容摘要一致 → 去重
    conflicts: int = 0        # 同序号内容冲突 → 隔离区间
    late_dropped: int = 0     # 超出补入窗口的迟到分片
    missing: int = 0          # 清单引用但本地缺失的分片


def ingest(manifest: Manifest, lateness_window: float | None = None) -> TransportResult:
    """按清单顺序模拟分片到达，产出有序投递、缺口与错误记录。

    - 重复：同序号且内容摘要一致 → 去重计数；
    - 冲突：同序号但内容摘要不同 → 报告错误并隔离整个区间（两份都不用）；
    - 乱序：按媒体时间重排；迟到（到达时其时间窗已提交超窗）→ 丢弃计数；
    - 缺失：清单引用但文件不存在 → 记录缺口，保留声明时长。
    """
    window = lateness_window if lateness_window is not None else 2.0 * manifest.target_duration
    result = TransportResult()
    by_seq: dict[int, Delivery] = {}
    poisoned: set[int] = set()  # 已隔离（冲突）的序号
    committed = 0.0             # 已提交的最大媒体时间（单调推进）

    for ref in manifest.segments:
        if ref.sequence in poisoned:
            continue  # 冲突区间的后续到达一并丢弃
        if ref.end <= committed - window:
            result.late_dropped += 1
            result.errors.append(MediaError(
                "transport", f"迟到分片超出补入窗口，已丢弃（序号 {ref.sequence}）",
                ref.uri, (ref.start, ref.end)))
            continue
        path = manifest.base_dir / ref.uri
        if not path.is_file():
            result.missing += 1
            result.gaps.append(Gap(ref.start, ref.duration, "missing"))
            result.errors.append(MediaError(
                "transport", f"分片文件缺失: {ref.uri}", ref.uri, (ref.start, ref.end)))
            continue
        data = path.read_bytes()
        delivery = Delivery(ref, path, hashlib.sha256(data).hexdigest(), len(data))
        prev = by_seq.get(ref.sequence)
        if prev is not None:
            if prev.digest == delivery.digest:
                result.duplicates += 1
                continue
            # 同序号内容冲突：报告错误并隔离冲突区间
            result.conflicts += 1
            del by_seq[ref.sequence]
            poisoned.add(ref.sequence)
            first = prev.ref
            result.gaps.append(Gap(first.start, first.duration, "conflict"))
            result.errors.append(MediaError(
                "transport",
                f"序号 {ref.sequence} 内容冲突（{first.uri} vs {ref.uri}），区间已隔离",
                ref.uri, (first.start, first.end)))
            continue
        by_seq[ref.sequence] = delivery
        committed = max(committed, ref.end)

    result.deliveries = sorted(by_seq.values(), key=lambda d: (d.ref.start, d.ref.sequence))
    result.gaps.sort(key=lambda g: g.start)
    return result
