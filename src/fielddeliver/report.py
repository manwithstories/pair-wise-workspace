"""指标聚合、JSON 报告与终端输出。

验收六项指标：
  sync_error_ms / packet_recovery_rate / p95_latency_ms /
  random_seek_pass_rate / audio_sample_delta / color_conflicts
另报告可恢复/不可恢复包数、缺口与各轨裁决。退出码由问题严重度聚合裁决。
"""
from __future__ import annotations

import json
import math
from dataclasses import asdict, dataclass, field

from .model import (ErrorCategory, ExitCode, Issue, Manifest, TrackResult,
                    atomic_write_json)
from .timeline import Timeline


@dataclass(slots=True)
class Metrics:
    sync_error_ms: float = 0.0
    packet_recovery_rate: float = 0.0
    p95_latency_ms: float = 0.0
    random_seek_pass_rate: float = 0.0
    audio_sample_delta: int = 0
    color_conflicts: int = 0
    recovered_packets: int = 0
    unrecovered_packets: int = 0
    total_loss_events: int = 0
    track_count: int = 0
    duration_us: int = 0
    # 成功样例（仅含可恢复事件的轨道）与损坏样例分组统计。
    healthy_recovery_rate: float = 1.0
    damaged_recovery_rate: float = 0.0
    damaged_unrecovered: int = 0
    thresholds: dict = field(default_factory=dict)

    def to_dict(self) -> dict:
        return {k: getattr(self, k) for k in (
            "sync_error_ms", "packet_recovery_rate", "p95_latency_ms",
            "random_seek_pass_rate", "audio_sample_delta", "color_conflicts",
            "recovered_packets", "unrecovered_packets", "total_loss_events",
            "healthy_recovery_rate", "damaged_recovery_rate",
            "damaged_unrecovered", "track_count", "duration_us",
            "thresholds")}


def _percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    k = min(len(s) - 1, max(0, math.ceil(pct / 100.0 * len(s)) - 1))
    return s[k]


def _track_recovery(result: TrackResult) -> tuple[int, int]:
    """返回单轨的 (已恢复事件数, 不可恢复事件数)。"""
    recovered = sum(
        1 for i in result.issues
        if i.category is ErrorCategory.PACKET_LOSS_RECOVERED
        or (i.category in (ErrorCategory.TIMESTAMP_REPAIRED,
                           ErrorCategory.DUPLICATE_FRAME)
            and i.recovered))
    unrecovered = sum(
        1 for i in result.issues
        if i.category in (ErrorCategory.PACKET_LOSS_UNRECOVERED,
                          ErrorCategory.TRUNCATED_STREAM,
                          ErrorCategory.DECODE_FAILURE,
                          ErrorCategory.TIMESTAMP_GAP)
        and i.recovered is not True)
    return recovered, unrecovered


def aggregate(manifest: Manifest, results: dict[str, TrackResult],
              timeline: Timeline, seek_rates: dict[str, float],
              audio_deltas: dict[str, int], sync_error_ms: float,
              color_conflict_count: int) -> Metrics:
    total_rec = total_unrec = 0
    healthy_rec = healthy_tot = 0
    damaged_rec = damaged_unrec = 0
    for result in results.values():
        rec, unrec = _track_recovery(result)
        total_rec += rec
        total_unrec += unrec
        if unrec > 0:                       # 损坏样例
            damaged_rec += rec
            damaged_unrec += unrec
        elif rec > 0:                       # 成功样例（只有可恢复事件）
            healthy_rec += rec
            healthy_tot += rec
    total_loss = total_rec + total_unrec

    rates = [v for v in seek_rates.values()]
    seek_rate = sum(rates) / len(rates) if rates else 1.0
    latencies = [x for r in results.values() for x in r.latency_ms]

    m = Metrics(
        sync_error_ms=round(sync_error_ms, 3),
        packet_recovery_rate=round(total_rec / total_loss, 4)
        if total_loss else 1.0,
        p95_latency_ms=round(_percentile(latencies, 95), 3),
        random_seek_pass_rate=round(seek_rate, 4),
        audio_sample_delta=sum(audio_deltas.values()),
        color_conflicts=color_conflict_count,
        recovered_packets=total_rec,
        unrecovered_packets=total_unrec,
        total_loss_events=total_loss,
        healthy_recovery_rate=round(healthy_rec / healthy_tot, 4)
        if healthy_tot else 1.0,
        damaged_recovery_rate=round(
            damaged_rec / (damaged_rec + damaged_unrec), 4)
        if (damaged_rec + damaged_unrec) else 0.0,
        damaged_unrecovered=damaged_unrec,
        track_count=len(results),
        duration_us=timeline.duration_us())
    m.thresholds = default_thresholds(m.duration_us)
    return m


def default_thresholds(duration_us: int) -> dict:
    return {
        "sync_error_ms_max": 20.0,
        "packet_recovery_rate_min": 0.90,
        "random_seek_pass_rate_min": 0.99,
        "p95_latency_ms_max": round(duration_us / 1000.0 * 0.5, 3),
    }


def decide_exit_code(manifest: Manifest, results: dict[str, TrackResult],
                     metrics: Metrics, hard_input_error: ExitCode | None
                     ) -> ExitCode:
    """按根因类别裁决退出码。

    四类退出码是并列的失败模式而非数值严重度排序：
    - UPSTREAM_FAILED 只是级联标记，不决定退出码（根因在上游轨）；
    - inherit 策略下已消解的色彩冲突不阻断（error 策略在调度前已抛 2）；
    - 内部错误(1)优先于素材不可恢复(3)，因为它代表流水线自身缺陷。
    """
    if hard_input_error is not None:
        return hard_input_error
    internal = invalid = unrecoverable = False
    for r in results.values():
        for issue in r.issues:
            cat = issue.category
            if cat is ErrorCategory.UPSTREAM_FAILED:
                continue
            if issue.recovered and cat.exit_code is ExitCode.OK:
                continue
            if cat is ErrorCategory.COLOR_CONFLICT:
                continue  # inherit 已消解；error 策略走 hard_input_error
            if cat.exit_code is ExitCode.INTERNAL:
                internal = True
            elif cat.exit_code is ExitCode.INVALID_INPUT:
                invalid = True
            elif cat.exit_code is ExitCode.UNRECOVERABLE:
                unrecoverable = True
    if internal:
        return ExitCode.INTERNAL
    if invalid:
        return ExitCode.INVALID_INPUT
    if unrecoverable:
        return ExitCode.UNRECOVERABLE
    return ExitCode.OK


def build_report(manifest: Manifest, results: dict[str, TrackResult],
                 timeline: Timeline, metrics: Metrics,
                 color_verdicts: dict) -> dict:
    return {
        "project": manifest.project,
        "schema_version": 1,
        "metrics": metrics.to_dict(),
        "timeline": {
            "duration_us": timeline.duration_us(),
            "video_duration_us": timeline.video_duration_us(),
            "order": timeline.order,
            "segments": [
                {**asdict(s), "kind": s.kind.value}
                for s in timeline.segments],
            "gaps": [asdict(g) for g in timeline.gaps],
        },
        "tracks": {tid: r.to_dict() for tid, r in results.items()},
        "color_verdicts": {
            tid: {
                "merged": v.merged(),
                "decisions": [asdict(d) for d in v.decisions],
                "conflicts": [i.to_dict() for i in v.conflicts],
            } for tid, v in color_verdicts.items()},
    }


def write_report(report: dict, path: str) -> None:
    atomic_write_json(path, report)


def render_terminal(metrics: Metrics, exit_code: ExitCode) -> str:
    th = metrics.thresholds
    lines = [
        "fielddeliver verify 报告",
        "=" * 48,
        f"  sync_error_ms            : {metrics.sync_error_ms:>10.3f}   "
        f"(阈值 <= {th.get('sync_error_ms_max')})",
        f"  packet_recovery_rate     : {metrics.packet_recovery_rate:>10.4f}   "
        f"(成功样例 {metrics.healthy_recovery_rate:.4f} "
        f">= {th.get('packet_recovery_rate_min')})",
        f"  p95_latency_ms           : {metrics.p95_latency_ms:>10.3f}   "
        f"(阈值 <= {th.get('p95_latency_ms_max')})",
        f"  random_seek_pass_rate    : {metrics.random_seek_pass_rate:>10.4f}   "
        f"(阈值 >= {th.get('random_seek_pass_rate_min')})",
        f"  audio_sample_delta       : {metrics.audio_sample_delta:>10d}",
        f"  color_conflicts          : {metrics.color_conflicts:>10d}",
        "-" * 48,
        f"  丢包事件 总计/已恢复/不可恢复: "
        f"{metrics.total_loss_events}/{metrics.recovered_packets}/"
        f"{metrics.unrecovered_packets}",
        f"  损坏样例恢复率: {metrics.damaged_recovery_rate:.4f}   "
        f"损坏样例不可恢复数: {metrics.damaged_unrecovered}",
        f"  轨道数: {metrics.track_count}   素材时长: "
        f"{metrics.duration_us/1e6:.3f}s",
        f"  退出码: {int(exit_code)} ({exit_code.name})",
    ]
    return "\n".join(lines)


def to_json_stdout(report: dict, exit_code: ExitCode) -> str:
    return json.dumps({"exit_code": int(exit_code), **report},
                      ensure_ascii=False, indent=2)
