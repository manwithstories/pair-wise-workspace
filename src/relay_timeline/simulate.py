"""传输模拟：打包、故障注入（重复/乱序/突发丢失/带宽分段）与样例生成。

全部为本地确定性模拟，不访问任何真实外部节点。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .packet_repair import Packet
from .timebase import TimeBase
from .timeline import Clip, Timeline

PACKET_SIZE = 4096


@dataclass
class FaultProfile:
    duplicate_rate: float = 0.0      # 每个包被复制一次的概率
    reorder_window: int = 0          # 局部乱序窗口大小，0 关闭
    burst_loss_events: int = 0       # 突发丢失次数
    burst_loss_len: int = 4          # 每次突发丢失的包数
    # 分段带宽：(占总时长比例, bps)；带宽越低丢包概率越高
    bandwidth_segments: list[tuple[float, int]] = field(
        default_factory=lambda: [(1.0, 8_000_000)]
    )


def packetize(segment_id: int, data: bytes, start_seq: int,
              timestamp_ticks: int) -> list[Packet]:
    """把一段已编码媒体切为定长包。"""
    packets = []
    for i in range(0, len(data), PACKET_SIZE):
        packets.append(
            Packet(
                seq=start_seq + len(packets),
                segment_id=segment_id,
                packet_index=len(packets),
                payload=data[i : i + PACKET_SIZE],
                timestamp_ticks=timestamp_ticks,
            )
        )
    return packets


def inject_faults(packets: list[Packet], profile: FaultProfile,
                  rng: np.random.Generator) -> list[Packet]:
    """按配置注入突发丢失、重复与乱序；带宽分段调节丢包概率。"""
    stream = list(packets)

    # 1) 突发丢失：随机选起点，整段剔除
    for _ in range(profile.burst_loss_events):
        if len(stream) <= profile.burst_loss_len + 1:
            break
        start = int(rng.integers(0, len(stream) - profile.burst_loss_len))
        del stream[start : start + profile.burst_loss_len]

    # 2) 带宽分段：低带宽段按概率额外丢包（单包，可恢复缺口）
    if len(profile.bandwidth_segments) > 1 or profile.bandwidth_segments[0][1] < 8_000_000:
        n = len(stream)
        kept: list[Packet] = []
        for i, pkt in enumerate(stream):
            frac = i / max(n, 1)
            bps = _bandwidth_at(profile.bandwidth_segments, frac)
            drop_p = 0.0 if bps >= 8_000_000 else min(0.02, 2_000_000 / bps * 0.01)
            if rng.random() >= drop_p:
                kept.append(pkt)
        stream = kept

    # 3) 重复：按概率复制包（副本紧随原包）
    if profile.duplicate_rate > 0:
        duped: list[Packet] = []
        for pkt in stream:
            duped.append(pkt)
            if rng.random() < profile.duplicate_rate:
                duped.append(pkt)
        stream = duped

    # 4) 局部乱序：窗口内随机置换（稳定重排前的到达次序）
    if profile.reorder_window > 1:
        w = profile.reorder_window
        for off in range(0, len(stream), w):
            window = stream[off : off + w]
            rng.shuffle(window)
            stream[off : off + w] = window

    for arrival, pkt in enumerate(stream):
        object.__setattr__(pkt, "arrival", arrival)
    return stream


def _bandwidth_at(segments: list[tuple[float, int]], frac: float) -> int:
    acc = 0.0
    for share, bps in segments:
        acc += share
        if frac <= acc:
            return bps
    return segments[-1][1]


def sample_timeline(tb: TimeBase, duration_s: float = 12.0) -> Timeline:
    """生成样例时间线：双轨、硬切与一段定长交叉淡化。"""
    total = tb.to_ticks(duration_s)
    half = total // 2
    quarter = total // 4
    transition = tb.to_ticks(0.5)
    clips = [
        Clip(track=0, start=0, duration=half, source_id="cam-a"),
        Clip(track=0, start=half, duration=total - half, source_id="cam-b"),
        Clip(track=1, start=quarter, duration=half,
             source_id="overlay", transition_in=transition),
    ]
    timeline = Timeline(tb=tb, duration=total, clips=clips)
    timeline.validate()
    return timeline


FAULT_CASES: dict[str, FaultProfile] = {
    "normal": FaultProfile(),
    "duplicate-reorder": FaultProfile(duplicate_rate=0.15, reorder_window=6),
    "burst-loss": FaultProfile(burst_loss_events=2, burst_loss_len=4),
    "bandwidth-drop": FaultProfile(
        bandwidth_segments=[(0.5, 8_000_000), (0.5, 400_000)]
    ),
    "mixed": FaultProfile(
        duplicate_rate=0.1, reorder_window=4,
        burst_loss_events=1, burst_loss_len=3,
        bandwidth_segments=[(0.6, 8_000_000), (0.4, 600_000)],
    ),
}
