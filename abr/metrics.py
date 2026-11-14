"""指标聚合：切换次数、上下档振幅、平均码率、启动时延与 QoE。"""

from __future__ import annotations

from dataclasses import asdict, dataclass

from .buffer import BufferLedger
from .simulator import SegmentRecord

QOE_REBUFFER_WEIGHT = 4.3   # 每秒耗尽的惩罚
QOE_SWITCH_WEIGHT = 1.0     # 每次切换的惩罚


@dataclass(frozen=True)
class Metrics:
    trace_seconds: int
    segments: int
    switch_count: int
    switch_amplitude_kbps: int
    avg_bitrate_kbps: float
    startup_ms: float
    rebuffer_events: int
    rebuffer_total_ms: float
    paused_ms: float
    qoe: float
    qoe_bitrate_term: float
    qoe_rebuffer_penalty: float
    qoe_switch_penalty: float

    def to_dict(self) -> dict:
        return asdict(self)


def aggregate(
    records: list[SegmentRecord],
    buffer: BufferLedger,
    trace_seconds: int,
    startup_ms: float,
) -> Metrics:
    bitrates = [r.bitrate_kbps for r in records]
    switch_count = sum(1 for a, b in zip(bitrates, bitrates[1:]) if a != b)
    amplitude = sum(abs(b - a) for a, b in zip(bitrates, bitrates[1:]) if a != b)
    avg_bitrate = sum(bitrates) / len(bitrates)
    bitrate_term = avg_bitrate / 1000.0
    rebuffer_penalty = QOE_REBUFFER_WEIGHT * buffer.rebuffer_ms / 1000.0
    switch_penalty = QOE_SWITCH_WEIGHT * switch_count
    qoe = bitrate_term - rebuffer_penalty - switch_penalty
    return Metrics(
        trace_seconds=trace_seconds,
        segments=len(records),
        switch_count=switch_count,
        switch_amplitude_kbps=amplitude,
        avg_bitrate_kbps=round(avg_bitrate, 3),
        startup_ms=round(startup_ms, 3),
        rebuffer_events=buffer.rebuffer_events,
        rebuffer_total_ms=round(buffer.rebuffer_ms, 3),
        paused_ms=round(buffer.paused_ms, 3),
        qoe=round(qoe, 6),
        qoe_bitrate_term=round(bitrate_term, 6),
        qoe_rebuffer_penalty=round(rebuffer_penalty, 6),
        qoe_switch_penalty=round(switch_penalty, 6),
    )
