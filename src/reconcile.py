"""回批（reconcile）：展开 -> 定位 -> 校验 -> 生成套底条目与报告数据。

产出确定性：条目按稳定键排序、编号连续；同一输入重复运行结果逐字节一致。
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Dict, List, Sequence, Tuple

from .model import (
    AMBIGUOUS, ERROR, WARNING, ConformEntry, ExchangeDoc, Issue,
    LeafClip, LOCATED, MISSING,
)
from .resolver import AssetResolver, LocateResult
from .timeline import expand


@dataclass(frozen=True)
class ReconcileReport:
    entries: Tuple[ConformEntry, ...]
    issues: Tuple[Issue, ...]
    entry_count: int
    located_count: int
    missing_count: int
    ambiguous_count: int
    drift_total: int
    drift_max: int
    max_depth: int
    has_error: bool


def _overlap_checks(leaves: Sequence[LeafClip]) -> List[Issue]:
    """同轨道叶子区间重叠 -> warning（嵌套展开后坐标均在根时间轴）。"""
    issues: List[Issue] = []
    by_track: Dict[str, List[LeafClip]] = {}
    for lf in leaves:
        by_track.setdefault(lf.track_id, []).append(lf)
    for track_id, items in by_track.items():
        items = sorted(items, key=lambda x: (x.dst_in, x.dst_out))
        for prev, cur in zip(items, items[1:]):
            if cur.dst_in < prev.dst_out:
                issues.append(Issue(
                    WARNING, "overlap",
                    f"轨道 {track_id} 上条目 {prev.entry_id} 与 {cur.entry_id} "
                    f"区间重叠 ({cur.dst_in} < {prev.dst_out})",
                    entry_id=cur.entry_id))
    return issues


def reconcile(manifest: ExchangeDoc, decision: ExchangeDoc,
              media_root) -> ReconcileReport:
    expansion = expand(decision)
    leaves = expansion.leaves
    asset_map = manifest.asset_map()

    resolver = AssetResolver(media_root)
    resolver.build_index()

    entries: List[ConformEntry] = []
    issues: List[Issue] = list(expansion.issues)

    # 先定位，保证同素材的多个叶子共享同一结论
    located_assets: set = set()
    missing_assets: set = set()
    ambiguous_assets: set = set()
    location_cache: Dict[str, LocateResult] = {}

    for lf in leaves:
        if lf.asset_id not in location_cache:
            location_cache[lf.asset_id] = resolver.locate(lf.asset_id, asset_map)
        loc = location_cache[lf.asset_id]
        if loc.status == LOCATED:
            located_assets.add(lf.asset_id)
        elif loc.status == AMBIGUOUS:
            ambiguous_assets.add(lf.asset_id)
        else:
            missing_assets.add(lf.asset_id)

    for lf in leaves:
        loc = location_cache[lf.asset_id]
        entry_issues: List[Issue] = []

        if loc.status == MISSING:
            entry_issues.append(Issue(
                ERROR, "asset_missing",
                f"素材 {lf.asset_id} 无法在媒体根目录定位",
                entry_id=lf.entry_id, asset_id=lf.asset_id))
        elif loc.status == AMBIGUOUS:
            entry_issues.append(Issue(
                ERROR, "asset_ambiguous",
                f"素材 {lf.asset_id} 存在 {len(loc.candidates)} 个指纹候选，拒绝静默选择: "
                f"{', '.join(loc.candidates)}",
                entry_id=lf.entry_id, asset_id=lf.asset_id))

        # 源时长不足
        asset = asset_map.get(lf.asset_id)
        if asset is not None and asset.duration_frames is not None:
            need = lf.src_out
            if need > asset.duration_frames:
                entry_issues.append(Issue(
                    ERROR, "source_too_short",
                    f"素材 {lf.asset_id} 源出点 {need} 超过声明时长 "
                    f"{asset.duration_frames}",
                    entry_id=lf.entry_id, asset_id=lf.asset_id))

        # 时间基不一致（叶子时间基 vs 素材声明时间基）
        if asset is not None and asset.timebase is not None and \
                asset.timebase.rate != lf.timebase.rate:
            entry_issues.append(Issue(
                WARNING, "timebase_mismatch",
                f"素材 {lf.asset_id} 时间基 {asset.timebase} 与片段 {lf.timebase} 不一致",
                entry_id=lf.entry_id, asset_id=lf.asset_id))

        # 变速累计误差
        if lf.drift_frames > 0:
            entry_issues.append(Issue(
                WARNING, "speed_drift",
                f"条目 {lf.entry_id} 变速边界取整累计偏差 {lf.drift_frames} 帧",
                entry_id=lf.entry_id, asset_id=lf.asset_id))

        entries.append(ConformEntry(
            entry_id=lf.entry_id,
            asset_id=lf.asset_id,
            src_in=lf.src_in,
            src_out=lf.src_out,
            target_duration=lf.dst_out - lf.dst_in,
            track_id=lf.track_id,
            sequence_chain=lf.sequence_chain,
            locate_status=loc.status,
            drift_frames=lf.drift_frames,
            candidates=loc.candidates,
            issues=tuple(entry_issues),
        ))
        issues.extend(entry_issues)

    issues.extend(_overlap_checks(leaves))

    drift_total = sum(e.drift_frames for e in entries)
    drift_max = max((e.drift_frames for e in entries), default=0)
    issues = tuple(sorted(issues, key=lambda i: (
        0 if i.severity == ERROR else 1, i.code,
        i.entry_id if i.entry_id is not None else -1, i.message)))
    has_error = any(i.severity == ERROR for i in issues)

    return ReconcileReport(
        entries=tuple(entries),
        issues=issues,
        entry_count=len(entries),
        located_count=len(located_assets),
        missing_count=len(missing_assets),
        ambiguous_count=len(ambiguous_assets),
        drift_total=drift_total,
        drift_max=drift_max,
        max_depth=expansion.max_depth,
        has_error=has_error,
    )
