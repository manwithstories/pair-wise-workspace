"""JSON 报告生成、终端量化摘要与 verify 复核。

验收门限：av_sync_error_ms <= 40、recovery_rate >= 0.90、
p95_latency_ms <= 250。verify 重新计算文件与阶段产物校验值，
任一不匹配即判校验失败。退出码由 cli 依据 VerificationVerdict 汇总。
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field

from .timeline import ErrorKind, MediaError, Stage, sha256_hex

MAX_AV_SYNC_MS = 40.0
MIN_RECOVERY_RATE = 0.90
MAX_P95_LATENCY_MS = 250.0
REPORT_VERSION = 1


@dataclass(slots=True)
class ThresholdFailure:
    metric: str
    actual: float
    required: str
    message: str

    def to_dict(self) -> dict:
        return {"metric": self.metric, "actual": self.actual,
                "required": self.required, "message": self.message}


@dataclass(slots=True)
class VerificationVerdict:
    ok: bool
    checksum_ok: bool
    metric_ok: bool
    failures: list[ThresholdFailure] = field(default_factory=list)
    checksum_errors: list[str] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "ok": self.ok,
            "checksum_ok": self.checksum_ok,
            "metric_ok": self.metric_ok,
            "failures": [f.to_dict() for f in self.failures],
            "checksum_errors": self.checksum_errors,
        }


def build_report(result: dict, *, source_index_path: str | None = None) -> dict:
    """汇总流水线结果为可归档、可复核的 JSON 报告。"""

    transport = result["transport"]
    return {
        "report_version": REPORT_VERSION,
        "codec": result["codec"],
        "video": result["video"],
        "index_path": source_index_path,
        "metrics": {
            "av_sync_error_ms": transport["av_sync_error_ms"],
            "recovery_rate": transport["recovery_rate"],
            "p95_latency_ms": transport["p95_latency_ms"],
            "recovered_packets": transport["recovered_packets"],
            "total_packets": transport["total_packets"],
            "lost_packets": transport["lost_packets"],
            "duplicate_packets": transport["duplicate_packets"],
            "reordered_packets": transport["reordered_packets"],
            "reconnect_count": transport["reconnect_count"],
        },
        "unrecovered_ranges": transport["unrecovered_ranges"],
        "error_offsets": result["error_offsets"],
        "error_count": result["error_count"],
        "recovered_event_count": result.get("recovered_event_count", 0),
        "segments": result["segments"],
        "silence": result["silence"],
        "dedup": result["dedup"],
        "stages": result["stages"],
    }


def save_report(report: dict, path: str) -> None:
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(report, fh, ensure_ascii=False, indent=2, sort_keys=True)


def load_report(path: str) -> dict:
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def evaluate_metrics(report: dict) -> list[ThresholdFailure]:
    m = report["metrics"]
    failures: list[ThresholdFailure] = []
    if m["av_sync_error_ms"] > MAX_AV_SYNC_MS:
        failures.append(ThresholdFailure(
            "av_sync_error_ms", m["av_sync_error_ms"],
            f"<={MAX_AV_SYNC_MS}", "音画同步误差超标"))
    if m["recovery_rate"] < MIN_RECOVERY_RATE:
        failures.append(ThresholdFailure(
            "recovery_rate", m["recovery_rate"],
            f">={MIN_RECOVERY_RATE}", "恢复率不足"))
    if m["p95_latency_ms"] > MAX_P95_LATENCY_MS:
        failures.append(ThresholdFailure(
            "p95_latency_ms", m["p95_latency_ms"],
            f"<={MAX_P95_LATENCY_MS}", "P95 延迟超标"))
    return failures


def verify_checksums(report: dict) -> list[str]:
    """重算视频文件与各阶段输出校验值，返回不匹配说明列表（空为通过）。"""

    problems: list[str] = []
    video = report.get("video", {})
    path = video.get("path")
    if path and os.path.exists(path):
        actual = sha256_hex(open(path, "rb").read())
        if actual != video.get("sha256"):
            problems.append(
                f"视频校验值不匹配：报告 {video.get('sha256')[:12]}… "
                f"实际 {actual[:12]}…")
    elif path:
        problems.append(f"视频文件已不存在：{path}")
    for stage in report.get("stages", []):
        payload = json.dumps(stage.get("output", {}), ensure_ascii=False,
                             sort_keys=True).encode("utf-8")
        if stage.get("output_checksum") != sha256_hex(payload):
            problems.append(f"阶段 {stage.get('name')} 产物校验值不匹配")
    return problems


def verify_report(report: dict) -> VerificationVerdict:
    failures = evaluate_metrics(report)
    checksum_errors = verify_checksums(report)
    return VerificationVerdict(
        ok=not failures and not checksum_errors,
        checksum_ok=not checksum_errors,
        metric_ok=not failures,
        failures=failures,
        checksum_errors=checksum_errors,
    )


def has_unrecoverable(report: dict) -> bool:
    """传输阶段存在未恢复字节范围即视为不可恢复损坏。"""

    return bool(report.get("unrecovered_ranges"))


def format_summary(report: dict, verdict: VerificationVerdict | None = None) -> str:
    """生成终端量化摘要，字段名与验收关键字对齐。"""

    m = report["metrics"]
    lines = [
        "=" * 64,
        f"relay-media 中继验证摘要  (codec={report['codec']})",
        "-" * 64,
        f"  av_sync_error_ms   : {m['av_sync_error_ms']:.2f}   "
        f"[门限 <= {MAX_AV_SYNC_MS}]",
        f"  recovery_rate      : {m['recovery_rate'] * 100:.2f}%  "
        f"[门限 >= {MIN_RECOVERY_RATE * 100:.0f}%]",
        f"  p95_latency_ms     : {m['p95_latency_ms']:.2f}   "
        f"[门限 <= {MAX_P95_LATENCY_MS}]",
        f"  recovered_packets  : {m['recovered_packets']}/"
        f"{m['total_packets']}  (lost={m['lost_packets']}, "
        f"dup={m['duplicate_packets']}, reorder={m['reordered_packets']})",
        f"  reconnect_count    : {m['reconnect_count']}",
        f"  error_offsets      : {report['error_offsets']}",
        f"  error_count        : {report['error_count']} (未恢复)",
        f"  recovered_events   : {report['recovered_event_count']} (已修复)",
        f"  unrecovered_ranges : {report['unrecovered_ranges']}",
        "-" * 64,
    ]
    if verdict is not None:
        if verdict.ok:
            lines.append("  结论: 验证通过 ✓ (校验值与指标全部达标)")
        else:
            for f in verdict.failures:
                lines.append(f"  指标未达标: {f.metric}={f.actual} "
                             f"要求 {f.required} — {f.message}")
            for problem in verdict.checksum_errors:
                lines.append(f"  校验失败: {problem}")
    lines.append("=" * 64)
    return "\n".join(lines)
