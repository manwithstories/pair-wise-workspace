"""报表：终端表格与 JSON 输出。"""

from __future__ import annotations

import json

from .metrics import Metrics

SUMMARY_ROWS = (
    ("trace_seconds", "轨迹时长 (s)"),
    ("segments", "完成分片数"),
    ("switch_count", "切换次数"),
    ("switch_amplitude_kbps", "上下档振幅 (kbps)"),
    ("avg_bitrate_kbps", "平均码率 (kbps)"),
    ("startup_ms", "启动时延 (ms)"),
    ("rebuffer_events", "耗尽事件数"),
    ("rebuffer_total_ms", "耗尽总时长 (ms)"),
    ("paused_ms", "满缓冲暂停 (ms)"),
    ("qoe_bitrate_term", "QoE 码率项"),
    ("qoe_rebuffer_penalty", "QoE 耗尽惩罚"),
    ("qoe_switch_penalty", "QoE 切换惩罚"),
    ("qoe", "QoE 总分"),
)


def metrics_to_json(metrics: Metrics) -> str:
    return json.dumps(metrics.to_dict(), ensure_ascii=False, indent=2)


def format_summary(metrics: Metrics) -> str:
    data = metrics.to_dict()
    width = max(len(label) for _, label in SUMMARY_ROWS)
    lines = [f"  {label:<{width}}  {data[key]}" for key, label in SUMMARY_ROWS]
    return "模拟结果汇总\n" + "\n".join(lines)


def format_table(rows: list[dict], columns: list[str]) -> str:
    """把字典列表渲染为对齐的终端表格。"""
    if not rows:
        return "(no rows)"
    str_rows = [{c: str(row.get(c, "")) for c in columns} for row in rows]
    widths = {c: max(len(c), *(len(r[c]) for r in str_rows)) for c in columns}
    header = "  ".join(c.ljust(widths[c]) for c in columns)
    sep = "  ".join("-" * widths[c] for c in columns)
    body = ["  ".join(r[c].ljust(widths[c]) for c in columns) for r in str_rows]
    return "\n".join([header, sep, *body])
