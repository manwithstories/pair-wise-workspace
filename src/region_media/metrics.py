"""恢复率、同步误差与 P95 延迟指标汇总。

所有指标从同一错误模型与流水线计数器派生，保证处理阶段与最终
摘要口径一致；阈值目标集中在 :data:`TARGETS`，verify 与 CLI 共用。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .model import ErrorEvent, ErrorKind
from .sync import SyncReport

TARGETS = {
    "sync_error_ms_p95": 40.0,
    "packet_recovery_rate": 0.85,
    "latency_ms_p95": 500.0,
}


@dataclass
class PipelineCounters:
    packets_total: int = 0
    packets_lost: int = 0
    packets_recovered: int = 0
    duplicates: int = 0
    late_drops: int = 0
    rejected_segments: int = 0
    wrap_events: int = 0
    jump_events: int = 0
    # 恢复引擎口径：检测到的缺口总数与其中不可恢复数
    gap_missing: int = 0
    gap_unrecoverable: int = 0
    # 每包处理延迟（到达 -> 进入同步评估），单位 us
    latency_samples_us: list[int] = field(default_factory=list)
    errors: list[ErrorEvent] = field(default_factory=list)

    def error_counts(self) -> dict[str, int]:
        out: dict[str, int] = {}
        for evt in self.errors:
            out[evt.kind.value] = out.get(evt.kind.value, 0) + 1
        return out


def _p95(values: list[int] | np.ndarray) -> float:
    if not len(values):
        return 0.0
    return float(np.percentile(np.asarray(values, dtype=np.float64), 95))


def build_summary(counters: PipelineCounters, sync: SyncReport,
                  checkpoint_resumed: bool) -> dict:
    """生成 verify/CLI 共用的终端 JSON 摘要。"""
    # 恢复率统一采用恢复引擎口径：recovered / 检测到的缺口总数。
    # 缺口总数 = 已恢复 + 不可恢复；无缺口时为 1.0。窗口外迟到等
    # 不可恢复丢失计入 gap_unrecoverable，不与 conditioner 丢包口径混用。
    gap_total = counters.gap_missing
    recovery_rate = (counters.packets_recovered / gap_total
                     if gap_total else 1.0)
    latency_ms_p95 = round(_p95(counters.latency_samples_us) / 1000.0, 3)
    summary = {
        "sync_error_ms_p95": sync.p95_abs_ms,
        "sync_error_ms_max": sync.max_abs_ms,
        "sync_error_ms_mean": sync.mean_ms,
        "sync_samples": sync.samples,
        "vfr_detected": sync.vfr,
        "packet_recovery_rate": round(recovery_rate, 4),
        "latency_ms_p95": latency_ms_p95,
        "packets_total": counters.packets_total,
        "packets_lost": counters.packets_lost,
        "packets_recovered": counters.packets_recovered,
        "late_out_of_window": counters.late_drops,
        "duplicates": counters.duplicates,
        "wrap_events": counters.wrap_events,
        "jump_events": counters.jump_events,
        "rejected_segments": counters.rejected_segments,
        "checkpoint_resumed": checkpoint_resumed,
        "error_counts": counters.error_counts(),
        "sync_actions": sync.actions,
    }
    summary["targets_met"] = evaluate_targets(summary)
    return summary


def evaluate_targets(summary: dict) -> dict[str, bool]:
    return {
        "sync_error_ms_p95":
            summary["sync_error_ms_p95"] <= TARGETS["sync_error_ms_p95"],
        "packet_recovery_rate":
            summary["packet_recovery_rate"] >= TARGETS["packet_recovery_rate"],
        "latency_ms_p95":
            summary["latency_ms_p95"] <= TARGETS["latency_ms_p95"],
    }


def count_errors(errors: list[ErrorEvent], kind: ErrorKind) -> int:
    return sum(1 for e in errors if e.kind == kind)
