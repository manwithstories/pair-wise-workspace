"""包修复：去重、稳定重排、缺失区间标记。

输入为模拟的传输包流（可能含重复、乱序、突发丢失），输出按
(segment_id, packet_index) 稳定排序的包序列、缺失区间清单，以及
可恢复缺包 / 无法解码片段的分类结果。
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field

# 连续缺失达到该长度记为一次突发丢失事件
BURST_LOSS_MIN_RUN = 3
# 缺失段长度不超过该值视为可恢复缺包（可在渲染层填补）
RECOVERABLE_RUN_MAX = 2


@dataclass(frozen=True)
class Packet:
    """一个传输包。seq 为发送端全局序号，arrival 为到达次序。"""

    seq: int
    segment_id: int
    packet_index: int
    payload: bytes
    timestamp_ticks: int
    arrival: int = 0

    @property
    def key(self) -> tuple[int, int]:
        return (self.segment_id, self.packet_index)

    def digest(self) -> str:
        return hashlib.sha256(self.payload).hexdigest()


@dataclass
class RepairReport:
    packet_duplicate_count: int = 0
    packet_reorder_count: int = 0
    burst_loss_count: int = 0
    # (segment_id, start, end) 闭区间
    missing_ranges: list[tuple[int, int, int]] = field(default_factory=list)
    recoverable_gaps: list[tuple[int, int, int]] = field(default_factory=list)
    undecodable_segments: list[int] = field(default_factory=list)
    received_segments: list[int] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "packet_duplicate_count": self.packet_duplicate_count,
            "packet_reorder_count": self.packet_reorder_count,
            "burst_loss_count": self.burst_loss_count,
            "missing_ranges": [list(r) for r in self.missing_ranges],
            "recoverable_gaps": [list(r) for r in self.recoverable_gaps],
            "undecodable_segments": list(self.undecodable_segments),
            "received_segments": list(self.received_segments),
        }


def repair_packets(
    packets: list[Packet],
    expected_segments: dict[int, int],
) -> tuple[list[Packet], RepairReport]:
    """修复包流。

    expected_segments: {segment_id: 该段应有的包数}，来自传输头。
    返回 (按序去重后的包列表, 修复报告)。
    """
    report = RepairReport()

    # 1) 乱序统计：到达序号小于已见最大 seq 的包计为一次乱序
    max_seq = -1
    for pkt in packets:
        if pkt.seq < max_seq:
            report.packet_reorder_count += 1
        else:
            max_seq = pkt.seq

    # 2) 去重（保留首次到达），随后按 key 稳定排序
    seen: set[tuple[int, int]] = set()
    unique: list[Packet] = []
    for pkt in packets:
        if pkt.key in seen:
            report.packet_duplicate_count += 1
            continue
        seen.add(pkt.key)
        unique.append(pkt)
    unique.sort(key=lambda p: p.key)

    # 3) 逐段核对完整性，标记缺失区间与分类
    for segment_id in sorted(expected_segments):
        want = expected_segments[segment_id]
        have = {
            p.packet_index for p in unique if p.segment_id == segment_id
        }
        missing = sorted(set(range(want)) - have)
        if not missing:
            report.received_segments.append(segment_id)
            continue
        runs = _runs(missing)
        segment_unrecoverable = False
        for start, end in runs:
            report.missing_ranges.append((segment_id, start, end))
            run_len = end - start + 1
            if run_len >= BURST_LOSS_MIN_RUN:
                report.burst_loss_count += 1
                segment_unrecoverable = True
            elif run_len <= RECOVERABLE_RUN_MAX:
                report.recoverable_gaps.append((segment_id, start, end))
            else:
                segment_unrecoverable = True
        if segment_unrecoverable:
            report.undecodable_segments.append(segment_id)
        else:
            report.received_segments.append(segment_id)

    return unique, report


def _runs(indices: list[int]) -> list[tuple[int, int]]:
    """把升序整数列表折叠为闭区间游程 [(start, end), ...]。"""
    if not indices:
        return []
    runs: list[tuple[int, int]] = []
    start = prev = indices[0]
    for value in indices[1:]:
        if value == prev + 1:
            prev = value
            continue
        runs.append((start, prev))
        start = prev = value
    runs.append((start, prev))
    return runs
