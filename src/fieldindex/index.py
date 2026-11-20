"""可随机访问的分段索引：分阶段落盘、中断续建与 SHA-256 证据复核。

索引文件为 JSON Lines：首行清单（魔数/版本/参数），其后每段一行
``segment`` 清单行 + 若干 ``entry`` 条目行。写入先落临时行再刷盘，
中断后重读时未验证条目一律不生效；尾部损坏或版本不符只重建受影响分段。
"""
from __future__ import annotations

import hashlib
import json
import os
from fractions import Fraction
from pathlib import Path

from . import INDEX_MAGIC, INDEX_VERSION
from .model import IndexEntry, Segment, SourceRef


class IndexError_(Exception):
    """索引文件损坏或版本不符。"""


def canonical_digest(
    source_blobs: dict[str, bytes],
    refs: list[SourceRef],
    segment_id: int,
    start: Fraction,
    end: Fraction,
) -> str:
    """对分段覆盖的原始字节计算确定性 SHA-256（与分段边界一并归一）。"""

    hasher = hashlib.sha256()
    hasher.update(f"seg={segment_id}|".encode())
    hasher.update(str(start).encode())
    hasher.update(b"|")
    hasher.update(str(end).encode())
    hasher.update(b"|")
    for ref in sorted(refs, key=lambda r: (r.source, r.start, r.end)):
        blob = source_blobs[ref.source]
        hasher.update(ref.source.encode("utf-8"))
        hasher.update(b":")
        hasher.update(blob[ref.start:ref.end])
        hasher.update(b";")
    return hasher.hexdigest()


class SegmentIndex:
    """分段索引构建器/读取器，支持中断续建与回读复核。"""

    def __init__(self, path: str | os.PathLike[str]) -> None:
        self.path = Path(path)
        self.segments: dict[int, Segment] = {}
        self.entries: dict[int, list[IndexEntry]] = {}
        self.meta: dict = {}

    # ---- 构建 --------------------------------------------------------

    def open_write(self, **meta) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._fh = self.path.open("w", encoding="utf-8")
        header = {"type": "header", "magic": INDEX_MAGIC,
                  "version": INDEX_VERSION, "meta": meta}
        self._fh.write(json.dumps(header, ensure_ascii=False, sort_keys=True) + "\n")
        self._fh.flush()
        os.fsync(self._fh.fileno())

    def open_append(self) -> None:
        """续建模式：在已验证的索引尾部追加新分段。"""

        self._fh = self.path.open("a", encoding="utf-8")

    def append_segment(
        self, segment: Segment, entries: list[IndexEntry]
    ) -> None:
        line = json.dumps(
            {"type": "segment", **segment.as_dict()},
            ensure_ascii=False, sort_keys=True,
        )
        self._fh.write(line + "\n")
        for entry in entries:
            self._fh.write(json.dumps(
                {"type": "entry", **entry.as_dict()},
                ensure_ascii=False, sort_keys=True,
            ) + "\n")
        self._fh.flush()
        os.fsync(self._fh.fileno())
        self.segments[segment.segment_id] = segment
        self.entries[segment.segment_id] = list(entries)

    def close_write(self) -> None:
        self._fh.close()

    # ---- 读取与续建 --------------------------------------------------

    def load_existing(self) -> set[int]:
        """读回已完整落盘的分段；返回可复用的 segment_id 集合。

        任何尾部损坏行/半截 JSON 都会中止读取并触发该分段重建，
        未通过验证的条目不会进入内存状态。
        """

        if not self.path.exists():
            return set()
        reusable: set[int] = set()
        current: Segment | None = None
        current_entries: list[IndexEntry] = []
        with self.path.open("r", encoding="utf-8") as fh:
            for lineno, line in enumerate(fh):
                if not line.endswith("\n"):
                    break  # 尾部截断：半截行所属分段不生效
                try:
                    obj = json.loads(line)
                except json.JSONDecodeError:
                    break
                if lineno == 0:
                    if obj.get("magic") != INDEX_MAGIC:
                        raise IndexError_("索引魔数不符")
                    if obj.get("version") != INDEX_VERSION:
                        raise IndexError_(
                            f"索引版本 {obj.get('version')} != {INDEX_VERSION}"
                        )
                    self.meta = obj.get("meta", {})
                    continue
                kind = obj.get("type")
                if kind == "segment":
                    if current is not None and self._block_complete(
                        current, current_entries
                    ):
                        self._keep(current, current_entries)
                        reusable.add(current.segment_id)
                    current = self._segment_from_dict(obj)
                    current_entries = []
                elif kind == "entry" and current is not None:
                    current_entries.append(self._entry_from_dict(obj))
        # 文件末尾的分段只有在条目数与清单一致时才可信
        if current is not None and self._block_complete(current, current_entries):
            self._keep(current, current_entries)
            reusable.add(current.segment_id)
        return reusable

    def verify(
        self, source_blobs: dict[str, bytes]
    ) -> tuple[int, int, list[int]]:
        """逐段复核 SHA-256，返回 (通过数, 总数, 失败分段 id 列表)。"""

        failed: list[int] = []
        for sid, segment in sorted(self.segments.items()):
            refs = [e.ref for e in self.entries[sid]]
            digest = canonical_digest(
                source_blobs, refs, sid, segment.start, segment.end
            )
            if digest == segment.sha256:
                segment.verified = True
            else:
                segment.verified = False
                failed.append(sid)
        total = len(self.segments)
        return total - len(failed), total, failed

    def random_access(self, segment_id: int) -> list[IndexEntry]:
        """按分段 id 随机定位条目；未验证分段拒绝返回。"""

        segment = self.segments.get(segment_id)
        if segment is None or not segment.verified:
            raise IndexError_(f"分段 {segment_id} 未验证，拒绝访问")
        return list(self.entries[segment_id])

    # ---- 序列化辅助 --------------------------------------------------

    @staticmethod
    def _block_complete(
        segment: Segment, entries: list[IndexEntry]
    ) -> bool:
        """条目数与分段清单一致才算完整，否则按未验证处理并触发重建。"""

        return len(entries) == segment.entry_count

    @staticmethod
    def _segment_from_dict(obj: dict) -> Segment:
        return Segment(
            segment_id=obj["segment_id"],
            start=Fraction(obj["start"]),
            end=Fraction(obj["end"]),
            entry_count=obj["entry_count"],
            byte_start=obj["byte_start"],
            byte_end=obj["byte_end"],
            sha256=obj["sha256"],
            verified=False,
        )

    @staticmethod
    def _entry_from_dict(obj: dict) -> IndexEntry:
        return IndexEntry(
            segment_id=obj["segment_id"],
            kind=obj["kind"],
            seq=obj["seq"],
            start=Fraction(obj["start"]),
            end=Fraction(obj["end"]),
            ref=SourceRef(**obj["ref"]),
            flags=list(obj.get("flags", [])),
        )

    def _keep(
        self, segment: Segment, entries: list[IndexEntry]
    ) -> None:
        self.segments[segment.segment_id] = segment
        self.entries[segment.segment_id] = entries
