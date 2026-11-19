"""弱网传输模拟：突发丢包、乱序、重复投递与节点重连恢复。

确定性建模（固定随机种子，重复运行结果一致）：
- packetize 按 MTU 把字节流切成等长（末包除外）包，保留源字节偏移；
- loss_burst 控制突发丢包比例，丢包以成簇 burst 发生而非独立散点；
- reorder/duplicate 概率产生乱序与重复投递；
- 重连后按最多 N 轮重传恢复缺失序号，每轮恢复率固定，仍缺失的字节
  计入 unrecovered_ranges；
- 指标：恢复字节比例、P95 重组延迟、音画同步误差（重排抖动上界）。
"""

from __future__ import annotations

import random
from dataclasses import dataclass, field

from .timeline import ErrorKind, MediaError, Stage

DEFAULT_MTU = 1200
_RETRANSMIT_ROUNDS = 3
_RETRANSMIT_RECOVERY = 0.75   # 每轮重传恢复 75% 的在途缺失
_BASE_LATENCY_MS = 32
_JITTER_MAX_MS = 48
_REORDER_PENALTY_MS = 28      # 乱序引入的额外到达延迟
_RECONNECT_PENALTY_MS = 95    # 重连后首个重传包的一次性代价
_REXMIT_ROUND_MS = 14         # 每轮重传递增延迟
_SYNC_JITTER_CAP_MS = 34      # 音画同步误差按乱序错位量计，封顶为抖动上界


@dataclass(slots=True)
class Packet:
    seq: int
    offset: int
    length: int
    pts_us: int | None = None

    @property
    def end_offset(self) -> int:
        return self.offset + self.length

    def to_dict(self) -> dict:
        return {"seq": self.seq, "offset": self.offset,
                "length": self.length, "pts_us": self.pts_us}


@dataclass(slots=True)
class ReceivedPacket:
    packet: Packet
    arrival_index: int
    latency_ms: float
    retransmitted: bool = False
    duplicate: bool = False

    def to_dict(self) -> dict:
        d = self.packet.to_dict()
        d.update({"arrival_index": self.arrival_index,
                  "latency_ms": round(self.latency_ms, 3),
                  "retransmitted": self.retransmitted,
                  "duplicate": self.duplicate})
        return d


@dataclass(slots=True)
class TransportResult:
    total_packets: int
    recovered_packets: int
    lost_packets: int
    duplicate_packets: int
    reordered_packets: int
    reconnect_count: int
    recovery_rate: float
    p95_latency_ms: float
    av_sync_error_ms: float
    unrecovered_ranges: list[tuple[int, int]]
    received: list[ReceivedPacket] = field(repr=False, default_factory=list)
    errors: list[MediaError] = field(default_factory=list)

    def metrics(self) -> dict:
        return {
            "total_packets": self.total_packets,
            "recovered_packets": self.recovered_packets,
            "lost_packets": self.lost_packets,
            "duplicate_packets": self.duplicate_packets,
            "reordered_packets": self.reordered_packets,
            "reconnect_count": self.reconnect_count,
            "recovery_rate": round(self.recovery_rate, 6),
            "p95_latency_ms": round(self.p95_latency_ms, 3),
            "av_sync_error_ms": round(self.av_sync_error_ms, 3),
            "unrecovered_ranges": [list(r) for r in self.unrecovered_ranges],
        }


def packetize(total_bytes: int, mtu: int = DEFAULT_MTU,
              pts_by_offset: dict[int, int] | None = None) -> list[Packet]:
    if mtu <= 0:
        raise ValueError("mtu 必须为正整数")
    pts_by_offset = pts_by_offset or {}
    packets: list[Packet] = []
    seq = 0
    offset = 0
    while offset < total_bytes:
        length = min(mtu, total_bytes - offset)
        pts = pts_by_offset.get(offset)
        packets.append(Packet(seq, offset, length, pts))
        offset += length
        seq += 1
    return packets


def simulate(
    total_bytes: int,
    *,
    loss_burst: float = 0.08,
    reorder_prob: float = 0.06,
    duplicate_prob: float = 0.04,
    mtu: int = DEFAULT_MTU,
    seed: int = 12789,
    pts_by_offset: dict[int, int] | None = None,
) -> TransportResult:
    if not 0.0 <= loss_burst <= 1.0:
        raise ValueError("loss_burst 必须落在 [0,1]")
    rng = random.Random(seed)
    packets = packetize(total_bytes, mtu, pts_by_offset)
    n = len(packets)

    lost_seqs = _burst_losses(rng, n, loss_burst)
    reorder_seqs = {p.seq for p in packets
                    if p.seq not in lost_seqs and rng.random() < reorder_prob}
    dup_seqs = {p.seq for p in packets
                if p.seq not in lost_seqs and rng.random() < duplicate_prob}

    # 重连 + 分轮重传恢复；记录每个重传包经历的轮次用于延迟建模
    reconnect_count = 1 if lost_seqs else 0
    remaining = set(lost_seqs)
    rexmit_round: dict[int, int] = {}
    for round_idx in range(1, _RETRANSMIT_ROUNDS + 1):
        if not remaining:
            break
        for seq in list(remaining):
            if rng.random() < _RETRANSMIT_RECOVERY:
                rexmit_round[seq] = round_idx
                remaining.remove(seq)
    unrecovered = sorted(remaining)

    received, p95, sync_err = _reassemble(
        packets, lost_seqs, rexmit_round, reorder_seqs, dup_seqs,
        reconnect_count, rng)

    recovered_packets = n - len(unrecovered)
    recovered_bytes = total_bytes - sum(
        packets[s].length for s in unrecovered)
    recovery_rate = recovered_bytes / total_bytes if total_bytes else 1.0

    errors: list[MediaError] = []
    for seq in unrecovered:
        pkt = packets[seq]
        errors.append(MediaError(
            Stage.TRANSPORT, ErrorKind.UNRECOVERED_RANGE, pkt.offset,
            f"序号 {seq} 重传 {_RETRANSMIT_ROUNDS} 轮后仍缺失",
            recovered=False, context={"seq": seq, "length": pkt.length}))

    ranges = [(packets[s].offset, packets[s].end_offset) for s in unrecovered]
    return TransportResult(
        total_packets=n,
        recovered_packets=recovered_packets,
        lost_packets=len(lost_seqs),
        duplicate_packets=len(dup_seqs),
        reordered_packets=len(reorder_seqs),
        reconnect_count=reconnect_count,
        recovery_rate=recovery_rate,
        p95_latency_ms=p95,
        av_sync_error_ms=sync_err,
        unrecovered_ranges=ranges,
        received=received,
        errors=errors,
    )


def _burst_losses(rng: random.Random, n: int, ratio: float) -> set[int]:
    target = round(n * ratio)
    lost: set[int] = set()
    while len(lost) < target:
        anchor = rng.randrange(n)
        # 突发长度 1..4，贴近弱网成簇丢包特征
        span = min(rng.randint(1, 4), target - len(lost))
        for k in range(span):
            seq = anchor + k
            if seq < n:
                lost.add(seq)
    return lost


def _reassemble(packets, lost, rexmit_round, reorder, dup,
                reconnect_count, rng):
    """按到达顺序重组，计算每包延迟与音画同步误差。

    延迟 = 基础 + 抖动（+乱序惩罚 / 重连与重传轮次代价）。
    音画同步误差只统计乱序导致的相对到达错位，封顶在抖动上界内，
    不把重传的绝对恢复延迟误记为同步漂移。
    """

    timeline: list[tuple[int, Packet, int | None, bool]] = []
    for pkt in packets:
        if pkt.seq in lost:
            if pkt.seq in rexmit_round:
                timeline.append((pkt.seq, pkt, rexmit_round[pkt.seq], False))
            continue
        timeline.append((pkt.seq, pkt, None, pkt.seq in dup))
        if pkt.seq in dup:
            timeline.append((pkt.seq, pkt, None, True))
    # 乱序：抽中序号与后一个交换到达位置
    order = list(timeline)
    reordered = 0
    for i, item in enumerate(order):
        if item[0] in reorder and i + 1 < len(order):
            order[i], order[i + 1] = order[i + 1], order[i]
            reordered += 1

    received: list[ReceivedPacket] = []
    latencies: list[float] = []
    sync_jitter = 0.0
    last_seq = -1
    for arrival, (seq, pkt, round_idx, is_dup) in enumerate(order):
        latency = _BASE_LATENCY_MS + rng.random() * _JITTER_MAX_MS
        if seq in reorder:
            latency += _REORDER_PENALTY_MS
        if round_idx is not None:
            latency += (_RECONNECT_PENALTY_MS
                        + round_idx * _REXMIT_ROUND_MS)
        received.append(ReceivedPacket(
            pkt, arrival, latency,
            retransmitted=round_idx is not None, duplicate=is_dup))
        if is_dup:
            continue
        latencies.append(latency)
        if seq < last_seq:
            # 乱序错位量：该包相对正常位置的额外延迟，封顶为抖动上界
            sync_jitter = min(
                _SYNC_JITTER_CAP_MS,
                max(sync_jitter, _REORDER_PENALTY_MS))
        last_seq = max(last_seq, seq)

    latencies_sorted = sorted(latencies)
    idx = min(len(latencies_sorted) - 1,
              int(round(0.95 * (len(latencies_sorted) - 1))))
    p95 = latencies_sorted[idx] if latencies_sorted else 0.0
    # 重连引入小幅固定同步偏移，叠加乱序抖动上界
    sync = sync_jitter + (6.0 if reconnect_count else 0.0)
    return received, p95, sync
