"""时间线展开：嵌套序列递归展平 + 变速后的源时间区间计算。

设计要点
--------
- 递归检测循环引用（祖先序列集合），命中即记 error 并剪枝，不无限展开。
- 叶子片段保留 ``sequence_chain`` / ``clip_chain`` 父子来源链与展开深度。
- 速率统一用 :class:`Fraction`；边界采用“半向上取整”（正 0.5 入、负 0.5 远离零）。
- 多段变速的累计误差 = 分段取整长度之和 − 总长度取整，恒为整数帧。
- 零速片段非法：记 error，产出占位叶子但不参与有效源区间。
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import List, Sequence, Set, Tuple

from .model import (
    Clip, ERROR, ExchangeDoc, Issue, LeafClip, Sequence, TimeBase,
)


def half_up(value: Fraction) -> int:
    """半向上取整：1/2 -> 1，-1/2 -> -1（负数沿绝对值方向对称进位）。"""
    if value >= 0:
        q, r = divmod(value, 1)
        return int(q) + (1 if r >= Fraction(1, 2) else 0)
    return -half_up(-value)


@dataclass(frozen=True)
class ExpansionResult:
    leaves: Tuple[LeafClip, ...]
    max_depth: int
    issues: Tuple[Issue, ...]


def _convert_frames(frames: int, src_tb: TimeBase, dst_tb: TimeBase) -> int:
    """把 src 时间基下的帧时长换算到 dst 时间基（半向上取整）。"""
    if src_tb.rate == dst_tb.rate:
        return frames
    return half_up(Fraction(frames) * dst_tb.rate / src_tb.rate)


def _segment_source_len(dst_len: int, rate: Fraction) -> int:
    """目标帧长度在给定速率下对应的源长度（带符号，半向上取整）。"""
    return half_up(Fraction(dst_len) / rate)


def _compute_leaf(clip: Clip, entry_id: int, track_id: str,
                  chain_seq: Tuple[str, ...], chain_clip: Tuple[str, ...],
                  depth: int, tb: TimeBase,
                  dst_in: int, dst_out: int) -> Tuple[LeafClip, List[Issue]]:
    issues: List[Issue] = []
    dst_len = dst_out - dst_in

    # 单速率快捷写法（含反向负速率）
    if not clip.speed_segments:
        rate = clip.speed if clip.speed is not None else Fraction(1, 1)
        if rate == 0:
            issues.append(Issue(ERROR, "zero_speed",
                                f"片段 {clip.clip_id} 速率为零，非法",
                                asset_id=clip.asset_id))
            leaf = LeafClip(entry_id, clip.asset_id or "", track_id, dst_in, dst_out,
                            clip.src_in, clip.src_in, rate, chain_seq, chain_clip,
                            depth, tb, 0)
            return leaf, issues
        signed = _segment_source_len(dst_len, rate)
        src_in, src_out = clip.src_in, clip.src_in + signed
        if rate < 0:
            src_in, src_out = clip.src_in + signed, clip.src_in
        return LeafClip(entry_id, clip.asset_id or "", track_id, dst_in, dst_out,
                        min(src_in, src_out), max(src_in, src_out), rate,
                        chain_seq, chain_clip, depth, tb, 0), issues

    # 多段变速：逐段计算并统计取整累计误差
    cursor = clip.src_in
    seg_rounded_sum = 0
    exact_source_sum = Fraction(0)
    rates: List[Fraction] = []
    zero_seen = False
    for seg in clip.speed_segments:
        local_dst = seg.dst_out - seg.dst_in
        rates.append(seg.rate)
        if seg.rate == 0:
            zero_seen = True
            issues.append(Issue(ERROR, "zero_speed",
                                f"片段 {clip.clip_id} 的变速段 {seg.dst_in}-{seg.dst_out} 速率为零",
                                asset_id=clip.asset_id))
            continue
        exact_len = Fraction(local_dst) / abs(seg.rate)
        exact_source_sum += exact_len
        signed = _segment_source_len(local_dst, seg.rate)
        seg_rounded_sum += abs(signed)
        cursor += signed
    # 累计取整误差：分段各自取整之和 − 精确总长再整体半向上取整（恒为整数帧）
    total_rounded = half_up(exact_source_sum)
    drift = abs(seg_rounded_sum - total_rounded)
    src_in = min(clip.src_in, cursor)
    src_out = max(clip.src_in, cursor)
    effective_rate = rates[0] if rates else Fraction(1, 1)
    leaf = LeafClip(entry_id, clip.asset_id or "", track_id, dst_in, dst_out,
                    src_in, src_out, effective_rate, chain_seq, chain_clip,
                    depth, tb, drift if not zero_seen else 0)
    return leaf, issues


def expand(doc: ExchangeDoc) -> ExpansionResult:
    """展开全部顶层序列，返回确定性排序后的叶子片段。"""
    seq_map = doc.sequence_map()
    leaves: List[LeafClip] = []
    issues: List[Issue] = []
    counter = [0]
    max_depth = [0]

    def next_id() -> int:
        counter[0] += 1
        return counter[0]

    def walk(seq: Sequence, ancestors: Set[str], chain_seq: Tuple[str, ...],
             chain_clip: Tuple[str, ...], depth: int,
             offset_root: int, parent_tb: TimeBase,
             track_id: str) -> None:
        max_depth[0] = max(max_depth[0], depth)
        if seq.sequence_id in ancestors:
            issues.append(Issue(
                ERROR, "cyclic_sequence",
                f"检测到循环引用: {' -> '.join(chain_seq + (seq.sequence_id,))}",
                sequence_id=seq.sequence_id))
            return
        ancestors = ancestors | {seq.sequence_id}
        chain_seq = chain_seq + (seq.sequence_id,)

        for track in seq.tracks:
            for clip in track.clips:
                # 子时间轴帧长 -> 根时间轴帧偏移
                span_local = clip.dst_out - clip.dst_in
                span_root = _convert_frames(span_local, seq.timebase, parent_tb)
                start_local_to_root = _convert_frames(clip.dst_in, seq.timebase, parent_tb)
                dst_in = offset_root + start_local_to_root
                dst_out = dst_in + span_root

                if clip.seq_ref is not None:
                    child = seq_map.get(clip.seq_ref)
                    if child is None:
                        issues.append(Issue(
                            ERROR, "sequence_not_found",
                            f"片段 {clip.clip_id} 引用了不存在的序列 {clip.seq_ref}",
                            sequence_id=clip.seq_ref))
                        continue
                    walk(child, ancestors, chain_seq, chain_clip + (clip.clip_id,),
                         depth + 1, dst_in, parent_tb, track.track_id)
                    continue

                leaf, leaf_issues = _compute_leaf(
                    clip, next_id(), track.track_id, chain_seq,
                    chain_clip + (clip.clip_id,), depth, seq.timebase, dst_in, dst_out)
                leaves.append(leaf)
                issues.extend(leaf_issues)

    for root_seq in doc.root_sequences():
        walk(root_seq, set(), (), (), 1, 0, root_seq.timebase, "")

    leaves.sort(key=lambda lf: (lf.dst_in, lf.track_id, lf.entry_id))
    # 重新编号，保证 entry_id 与稳定排序一致、重复运行不变
    reindexed = tuple(
        LeafClip(idx + 1, lf.asset_id, lf.track_id, lf.dst_in, lf.dst_out,
                 lf.src_in, lf.src_out, lf.rate, lf.sequence_chain,
                 lf.clip_chain, lf.depth, lf.timebase, lf.drift_frames)
        for idx, lf in enumerate(leaves)
    )
    issues = tuple(sorted(issues, key=lambda i: (i.code, i.message)))
    return ExpansionResult(reindexed, max_depth[0], issues)
