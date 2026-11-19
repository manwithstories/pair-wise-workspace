"""交付统计：码率、质量与对齐指标的汇总、终端输出、JSON 与阈值校验。"""
from __future__ import annotations

import json
from pathlib import Path

# 验收阈值：码率偏差绝对值、响度误差、对齐误差
THRESHOLDS = {
    "bitrate_delta_pct": 12.0,
    "loudness_error_lu": 1.5,
    "alignment_ms": 20.0,
}

METRIC_ORDER = [
    "target_bitrate_bps",
    "actual_bitrate_bps",
    "bitrate_delta_pct",
    "integrated_lufs",
    "true_peak_dbfs",
    "silence_ratio",
    "manifest_alignment_ms",
    "duplicate_segments",
    "recovered_gaps",
    "peak_buffer_bytes",
]

EXTRA_ORDER = [
    "isolated_segments",
    "late_dropped_segments",
    "missing_segments",
    "duration_delta_ms",
    "bitrate_basis",
    "gain_db",
]


def print_summary(report: dict) -> None:
    for key in METRIC_ORDER:
        print(f"{key}: {report.get(key)}")
    for key in EXTRA_ORDER:
        if key in report:
            print(f"{key}: {report[key]}")


def write_json(report: dict, path: str | Path) -> None:
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    Path(path).write_text(
        json.dumps(report, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )


def load_json(path: str | Path) -> dict:
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError("报告顶层必须是 JSON 对象")
    return data


def verify(report: dict) -> list[str]:
    """返回未达标项描述列表；空列表表示全部校验通过。"""
    failures: list[str] = []

    delta = report.get("bitrate_delta_pct")
    if delta is None or abs(delta) > THRESHOLDS["bitrate_delta_pct"]:
        failures.append(
            f"码率偏差 {delta} 超过 ±{THRESHOLDS['bitrate_delta_pct']}%"
            f"（依据: {report.get('bitrate_basis', 'unknown')}）"
        )

    target = report.get("target_lufs")
    lufs = report.get("integrated_lufs")
    if lufs is None or target is None or abs(lufs - target) > THRESHOLDS["loudness_error_lu"]:
        failures.append(
            f"响度误差超限：integrated_lufs={lufs} 目标 {target}"
            f"（容差 ±{THRESHOLDS['loudness_error_lu']} LU）"
        )

    align = report.get("manifest_alignment_ms")
    if align is None or align > THRESHOLDS["alignment_ms"]:
        failures.append(
            f"对齐误差 {align} ms 超过 {THRESHOLDS['alignment_ms']} ms"
        )

    if report.get("unrecoverable_errors"):
        failures.append(f"存在 {report['unrecoverable_errors']} 个不可恢复媒体错误")
    return failures
