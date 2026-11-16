"""模拟过程指标统计。

消费每个分段的 :class:`~abr.types.SegmentResult`，累计码率分布、切换
次数（区分主动升档与被迫降档）、真实下溢时长、保护状态时长、平均缓冲
与风险拒绝次数。统计结果可序列化为 JSON，且对相同事件流逐位一致。
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .types import DecisionKind, SegmentResult, SwitchReason


@dataclass(slots=True)
class SimulationMetrics:
    """一次完整模拟的汇总指标。"""

    segment_count: int = 0
    selected_bitrates: list[float] = field(default_factory=list)
    selected_indices: list[int] = field(default_factory=list)
    switch_count: int = 0
    upshift_count: int = 0
    downshift_count: int = 0
    forced_downshift_count: int = 0     # 被迫降档（feasibility / 保护）
    opportunistic_upshift_count: int = 0  # 主动升档
    rebuffer_seconds: float = 0.0       # 真实下溢总时长
    protection_seconds: float = 0.0     # 保护状态总时长
    risk_reject_count: int = 0
    cooldown_apply_count: int = 0
    failed_segment_count: int = 0
    estimator_samples: int = 0
    _buffer_sum: float = 0.0
    _buffer_n: int = 0

    @property
    def avg_buffer_seconds(self) -> float:
        return self._buffer_sum / self._buffer_n if self._buffer_n else 0.0

    @property
    def average_bitrate_kbps(self) -> float:
        if not self.selected_bitrates:
            return 0.0
        return sum(self.selected_bitrates) / len(self.selected_bitrates)

    def record(self, seg: SegmentResult) -> None:
        """消费一个分段结果并累计指标。"""
        self.segment_count += 1
        self.selected_bitrates.append(seg.chosen.bitrate_kbps)
        self.selected_indices.append(seg.chosen.index)
        self.rebuffer_seconds += seg.underflow_seconds
        if seg.in_protection:
            self.protection_seconds += seg.download_seconds
        if seg.failed:
            self.failed_segment_count += 1

        # 缓冲水位以下载前快照计入平均
        self._buffer_sum += seg.buffer_before_s
        self._buffer_n += 1

        dec = seg.decision
        if dec.risk_rejected:
            self.risk_reject_count += 1
        if dec.cooldown_applied:
            self.cooldown_apply_count += 1

        if dec.kind is DecisionKind.UPSHIFT:
            self.switch_count += 1
            self.upshift_count += 1
            if dec.reason is SwitchReason.OPPORTUNISTIC:
                self.opportunistic_upshift_count += 1
        elif dec.kind is DecisionKind.DOWNSHIFT:
            self.switch_count += 1
            self.downshift_count += 1
            if dec.reason in (
                SwitchReason.FEASIBILITY,
                SwitchReason.UNDERFLOW_PROTECTION,
            ):
                self.forced_downshift_count += 1

    def to_dict(self) -> dict:
        """序列化为可 JSON 化的扁平字典（字段名与演示契约对齐）。"""
        return {
            "segments": self.segment_count,
            "selected_bitrates": [round(b, 3) for b in self.selected_bitrates],
            "selected_indices": list(self.selected_indices),
            "switch_count": self.switch_count,
            "upshift_count": self.upshift_count,
            "downshift_count": self.downshift_count,
            "forced_downshift_count": self.forced_downshift_count,
            "opportunistic_upshift_count": self.opportunistic_upshift_count,
            "rebuffer_seconds": round(self.rebuffer_seconds, 4),
            "protection_seconds": round(self.protection_seconds, 4),
            "avg_buffer_seconds": round(self.avg_buffer_seconds, 4),
            "average_bitrate_kbps": round(self.average_bitrate_kbps, 3),
            "risk_reject_count": self.risk_reject_count,
            "cooldown_apply_count": self.cooldown_apply_count,
            "failed_segment_count": self.failed_segment_count,
            "estimator_samples": self.estimator_samples,
        }
