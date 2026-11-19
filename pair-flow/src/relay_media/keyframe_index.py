"""关键帧索引的校验、补建与不完整标记。

索引为本地 JSON：{"codec", "entries": [{"offset", "pts_us", ...}]}。
校验项：编码标识一致性、偏移范围、单调性、重复项。
缺页（扫描得到关键帧但索引缺失）时用码流扫描结果补建候选关键帧；
截断码流导致末页无法确认时，索引显式标记 incomplete。
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

from .bitstream import CODECS, ScanResult
from .timeline import ErrorKind, MediaError, Stage


@dataclass(slots=True)
class IndexEntry:
    offset: int
    pts_us: int | None = None
    source: str = "index"          # index / scanned
    repaired: bool = False

    def to_dict(self) -> dict:
        return {
            "offset": self.offset,
            "pts_us": self.pts_us,
            "source": self.source,
            "repaired": self.repaired,
        }

    @classmethod
    def from_dict(cls, data: dict) -> "IndexEntry":
        return cls(
            offset=int(data["offset"]),
            pts_us=data.get("pts_us"),
            source=data.get("source", "index"),
            repaired=bool(data.get("repaired", False)),
        )


@dataclass(slots=True)
class IndexDocument:
    codec: str
    entries: list[IndexEntry] = field(default_factory=list)
    incomplete: bool = False
    errors: list[MediaError] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "codec": self.codec,
            "incomplete": self.incomplete,
            "entries": [e.to_dict() for e in self.entries],
            "errors": [e.to_dict() for e in self.errors],
        }

    def save(self, path: str) -> None:
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(self.to_dict(), fh, ensure_ascii=False, indent=2,
                      sort_keys=True)

    @classmethod
    def load(cls, path: str) -> "IndexDocument":
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
        if not isinstance(data, dict) or "codec" not in data:
            raise ValueError("索引文件缺少 codec 字段或结构非法")
        return cls(
            codec=data["codec"],
            entries=[IndexEntry.from_dict(x) for x in data.get("entries", [])],
            incomplete=bool(data.get("incomplete", False)),
            errors=[MediaError.from_dict(x) for x in data.get("errors", [])],
        )


def validate_index(doc: IndexDocument, scan: ScanResult) -> IndexDocument:
    """就地校验索引，逐条登记问题并剔除非法项，合法项保留。"""

    if doc.codec != scan.codec:
        doc.errors.append(MediaError(
            Stage.INDEX, ErrorKind.CODEC_MISMATCH, -1,
            f"索引编码 {doc.codec!r} 与码流 {scan.codec!r} 不一致",
            recovered=False))
    # 先记录原始顺序中的非单调问题，再排序归一，避免乱序连累合法项被剔除。
    last = -1
    for entry in doc.entries:
        if entry.offset <= last:
            doc.errors.append(MediaError(
                Stage.INDEX, ErrorKind.INDEX_NON_MONOTONIC, entry.offset,
                f"非单调偏移 {entry.offset}（上一项 {last}）",
                recovered=True))
        last = max(last, entry.offset)
    seen: set[int] = set()
    kept: list[IndexEntry] = []
    for entry in sorted(doc.entries, key=lambda e: e.offset):
        off = entry.offset
        if off < 0 or off >= scan.total_bytes:
            doc.errors.append(MediaError(
                Stage.INDEX, ErrorKind.INDEX_OFFSET_OUT_OF_RANGE, off,
                f"偏移 {off} 超出码流范围 [0,{scan.total_bytes})",
                recovered=False))
            continue
        if off in seen:
            doc.errors.append(MediaError(
                Stage.INDEX, ErrorKind.INDEX_DUPLICATE, off,
                f"重复索引项 {off}", recovered=True))
            continue
        seen.add(off)
        kept.append(entry)
    doc.entries = kept
    return doc


def rebuild_index(doc: IndexDocument, scan: ScanResult) -> IndexDocument:
    """用码流扫描结果补建缺页，返回同一份索引文档。

    - 扫描关键帧偏移不在索引中的，作为 scanned 候选补入并标记 repaired；
    - 索引指向的偏移不是关键帧单元起始的，登记降级（保留条目）；
    - 码流被截断时末页不可确认，索引标记 incomplete。
    """

    keyframes = {u.start_offset for u in scan.units if u.keyframe}
    indexed = {e.offset for e in doc.entries}
    unit_starts = {u.start_offset: u for u in scan.units}

    for off in sorted(keyframes - indexed):
        doc.entries.append(IndexEntry(
            offset=off, source="scanned", repaired=True))
        doc.errors.append(MediaError(
            Stage.INDEX, ErrorKind.INDEX_MISSING_PAGE, off,
            "索引缺页，已由码流扫描补建候选关键帧", recovered=True))

    for entry in doc.entries:
        unit = unit_starts.get(entry.offset)
        if unit is None or not unit.keyframe:
            doc.errors.append(MediaError(
                Stage.INDEX, ErrorKind.DEGRADED_BOUNDARY, entry.offset,
                "索引偏移未对应关键帧单元，切片边界降级", recovered=True,
                context={"is_unit_start": unit is not None}))

    doc.entries.sort(key=lambda e: e.offset)
    if scan.truncated or any(u.truncated for u in scan.units):
        doc.incomplete = True
        doc.errors.append(MediaError(
            Stage.INDEX, ErrorKind.TRUNCATED_UNIT, scan.total_bytes,
            "码流截断，索引末页不完整", recovered=False))
    return doc


def build_index_from_scan(scan: ScanResult) -> IndexDocument:
    """无索引文件时的完整补建路径。"""

    doc = IndexDocument(codec=scan.codec)
    for off in scan.keyframe_offsets():
        doc.entries.append(IndexEntry(offset=off, source="scanned",
                                      repaired=True))
    if scan.truncated:
        doc.incomplete = True
    return doc


def empty_index(codec: str) -> IndexDocument:
    if codec not in CODECS:
        raise ValueError(f"不支持的编码标识: {codec!r}")
    return IndexDocument(codec=codec)
