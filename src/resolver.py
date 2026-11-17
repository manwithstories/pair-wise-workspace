"""素材多级定位。

定位优先级：素材 ID / 别名链 -> 规范化相对路径 -> 文件大小 + SHA-256 片段指纹。
多候选一律标记 ``ambiguous``，绝不静默选择。路径访问限制在指定根目录内，
任何越界路径（含符号链接逃逸）一律拒绝。
"""

from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Dict, List, Mapping, Optional, Tuple

from .model import (
    AMBIGUOUS, FP_CHUNK, Asset, LOCATED, MISSING,
)


@dataclass(frozen=True)
class FileRecord:
    rel_path: str               # 相对 root 的 POSIX 风格路径
    size: int
    fingerprint: str
    absolute: Path


@dataclass(frozen=True)
class LocateResult:
    status: str                 # located | missing | ambiguous
    rel_path: Optional[str]
    candidates: Tuple[str, ...]


def is_within(base: Path, target: Path) -> bool:
    """target 是否位于 base 之内（比较 resolve 后的绝对路径，防符号链接逃逸）。"""
    try:
        base_r = base.resolve(strict=False)
        target_r = target.resolve(strict=False)
    except OSError:
        return False
    return target_r == base_r or base_r in target_r.parents


def normalize_rel(rel: str) -> str:
    """规范化清单内相对路径：POSIX 分隔符、折叠 ./ 与 ../、去前导斜杠。"""
    p = PurePosixPath(rel.replace("\\", "/"))
    parts: List[str] = []
    for part in p.parts:
        if part in ("", "."):
            continue
        if part == "..":
            if parts:
                parts.pop()
            continue
        parts.append(part)
    return "/".join(parts)


def fingerprint_file(path: Path, offset: int = 0,
                     length: Optional[int] = None) -> str:
    """SHA-256 片段指纹；length=None 表示取到文件尾。每个文件只读取一次由调用方缓存。"""
    h = hashlib.sha256()
    remaining = length
    with path.open("rb") as fh:
        if offset:
            fh.seek(offset)
        while True:
            if remaining is not None:
                if remaining <= 0:
                    break
                chunk = fh.read(min(FP_CHUNK, remaining))
            else:
                chunk = fh.read(FP_CHUNK)
            if not chunk:
                break
            h.update(chunk)
            if remaining is not None:
                remaining -= len(chunk)
    return h.hexdigest()


class AssetResolver:
    """对媒体根目录建一次索引，随后按素材逐条定位。"""

    def __init__(self, root: Path) -> None:
        self.root = root
        self._by_path: Dict[str, FileRecord] = {}
        self._by_fp: Dict[str, List[FileRecord]] = {}
        self._fp_cache: Dict[str, str] = {}     # rel_path -> 指纹，单文件不重复计算

    # ---- 索引（流式扫描，不一次性载入文件内容） ----

    def build_index(self) -> None:
        if not self.root.exists() or not self.root.is_dir():
            return
        for dirpath, dirnames, filenames in os.walk(self.root):
            # 拒绝越界符号链接目录：不跟随
            dirnames[:] = sorted(
                d for d in dirnames
                if not os.path.islink(os.path.join(dirpath, d))
            )
            for name in sorted(filenames):
                abs_path = Path(dirpath) / name
                if abs_path.is_symlink() or not abs_path.is_file():
                    continue
                if not is_within(self.root, abs_path):
                    continue
                try:
                    size = abs_path.stat().st_size
                except OSError:
                    continue
                rel = normalize_rel(str(abs_path.relative_to(self.root)))
                fp = self._fingerprint_cached(abs_path, rel)
                rec = FileRecord(rel, size, fp, abs_path)
                self._by_path[rel] = rec
                self._by_fp.setdefault(fp, []).append(rec)

    def _fingerprint_cached(self, abs_path: Path, rel: str) -> str:
        if rel not in self._fp_cache:
            self._fp_cache[rel] = fingerprint_file(abs_path)
        return self._fp_cache[rel]

    # ---- 别名链 ----

    @staticmethod
    def _resolve_alias(asset_id: str,
                       asset_map: Mapping[str, Asset]) -> Optional[str]:
        """沿别名链多跳解析到真实素材 ID；成环或断链返回 None。"""
        seen: set = set()
        current = asset_id
        while current not in seen:
            seen.add(current)
            if current in asset_map:
                return current
            nxt = None
            for asset in asset_map.values():
                for alias in asset.aliases:
                    if alias.alias == current:
                        nxt = alias.target_id
                        break
                if nxt is not None:
                    break
            if nxt is None:
                return None
            current = nxt
        return None  # 别名链成环

    # ---- 单条定位 ----

    def locate(self, asset_id: str, asset_map: Mapping[str, Asset]) -> LocateResult:
        canonical = self._resolve_alias(asset_id, asset_map)
        if canonical is None:
            return LocateResult(MISSING, None, ())
        asset = asset_map[canonical]
        norm = normalize_rel(asset.rel_path)

        # 1) 先收集大小+指纹全部候选。多候选即歧义，绝不静默选择，
        #    即使其中一个路径恰好与清单声明路径相同也不例外。
        fp_hits = tuple(r for r in self._by_fp.get(asset.fingerprint, ())
                        if r.size == asset.size)
        if len(fp_hits) >= 2:
            cands = tuple(sorted(r.rel_path for r in fp_hits))
            return LocateResult(AMBIGUOUS, None, cands)

        # 2) 唯一指纹候选：路径一致或改名/迁移都算定位成功
        if len(fp_hits) == 1:
            only = fp_hits[0]
            return LocateResult(LOCATED, only.rel_path,
                                () if only.rel_path == norm else (only.rel_path,))

        # 3) 无指纹候选时回退到路径+大小（指纹算法演进场景仍可人工复核）
        rec = self._by_path.get(norm)
        if rec is not None and rec.size == asset.size:
            return LocateResult(LOCATED, rec.rel_path, ())
        return LocateResult(MISSING, None, ())
