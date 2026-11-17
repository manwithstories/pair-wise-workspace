"""终端摘要与 JSON 报告输出；以及 verify 子命令的独立复核逻辑。"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Dict, List, Tuple

from .model import ConformEntry, Issue
from .reconcile import ReconcileReport

SUMMARY_LABELS = (
    ("entry_count", "回批条目数"),
    ("located_count", "已定位素材数"),
    ("missing_count", "无法定位素材项"),
    ("ambiguous_count", "歧义项"),
    ("drift_total", "时长偏差总帧数"),
    ("drift_max", "最大偏差帧数"),
    ("max_depth", "嵌套展开层数"),
)


def render_terminal(report: ReconcileReport) -> str:
    lines = ["=== 套底回批摘要 ==="]
    for key, label in SUMMARY_LABELS:
        lines.append(f"{label}={getattr(report, key)}")
    err = sum(1 for i in report.issues if i.severity == "error")
    warn = sum(1 for i in report.issues if i.severity == "warning")
    lines.append(f"错误={err} 警告={warn}")
    lines.append("")
    for entry in report.entries:
        chain = " > ".join(entry.sequence_chain)
        lines.append(
            f"#{entry.entry_id:02d} [{entry.locate_status}] 素材={entry.asset_id} "
            f"轨道={entry.track_id} 源入点={entry.src_in} 源出点={entry.src_out} "
            f"目标时长={entry.target_duration} 链={chain}")
    if report.issues:
        lines.append("")
        lines.append("--- 问题明细 ---")
        for issue in report.issues:
            head = f"[{issue.severity.upper()}] {issue.code}"
            tail = f" (条目 {issue.entry_id})" if issue.entry_id is not None else ""
            lines.append(f"{head}{tail}: {issue.message}")
    return "\n".join(lines)


def _issue_to_dict(issue: Issue) -> Dict[str, Any]:
    return {
        "severity": issue.severity,
        "code": issue.code,
        "message": issue.message,
        "entry_id": issue.entry_id,
        "asset_id": issue.asset_id,
        "sequence_id": issue.sequence_id,
    }


def _entry_to_dict(entry: ConformEntry) -> Dict[str, Any]:
    return {
        "entry_id": entry.entry_id,
        "asset_id": entry.asset_id,
        "src_in": entry.src_in,
        "src_out": entry.src_out,
        "target_duration": entry.target_duration,
        "track_id": entry.track_id,
        "sequence_chain": list(entry.sequence_chain),
        "locate_status": entry.locate_status,
        "drift_frames": entry.drift_frames,
        "candidates": list(entry.candidates),
        "issues": [_issue_to_dict(i) for i in entry.issues],
    }


def report_to_dict(report: ReconcileReport) -> Dict[str, Any]:
    return {
        "schema": "conform-report/1",
        "summary": {key: getattr(report, key) for key, _ in SUMMARY_LABELS},
        "has_error": report.has_error,
        "entries": [_entry_to_dict(e) for e in report.entries],
        "issues": [_issue_to_dict(i) for i in report.issues],
    }


def write_json(report: ReconcileReport, out_path: Path) -> None:
    """稳定写出：sort_keys + 固定缩进 + 末尾换行，保证重复运行一致。"""
    payload = report_to_dict(report)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with out_path.open("w", encoding="utf-8", newline="\n") as fh:
        json.dump(payload, fh, ensure_ascii=False, indent=2, sort_keys=True)
        fh.write("\n")


# ---------- verify：从落盘 JSON 独立复核 ----------

def load_report_dict(path: Path) -> Dict[str, Any]:
    with path.open("r", encoding="utf-8") as fh:
        return json.load(fh)


def verify_report(data: Dict[str, Any]) -> Tuple[bool, List[str]]:
    """校验报告内部一致性，返回 (是否通过, 问题描述列表)。"""
    problems: List[str] = []
    if data.get("schema") != "conform-report/1":
        problems.append("报告 schema 不是 conform-report/1")
    summary = data.get("summary", {})
    entries = data.get("entries", [])
    issues = data.get("issues", [])

    if summary.get("entry_count") != len(entries):
        problems.append("entry_count 与条目数组长度不一致")
    located = {e["asset_id"] for e in entries if e["locate_status"] == "located"}
    missing = {e["asset_id"] for e in entries if e["locate_status"] == "missing"}
    ambiguous = {e["asset_id"] for e in entries if e["locate_status"] == "ambiguous"}
    if summary.get("located_count") != len(located):
        problems.append("located_count 与条目定位结果不一致")
    if summary.get("missing_count") != len(missing):
        problems.append("missing_count 与条目定位结果不一致")
    if summary.get("ambiguous_count") != len(ambiguous):
        problems.append("ambiguous_count 与条目定位结果不一致")
    drift_total = sum(e["drift_frames"] for e in entries)
    drift_max = max((e["drift_frames"] for e in entries), default=0)
    if summary.get("drift_total") != drift_total:
        problems.append("drift_total 与条目累计不一致")
    if summary.get("drift_max") != drift_max:
        problems.append("drift_max 与条目最大值不一致")
    if any(i["severity"] == "error" for i in issues) != data.get("has_error"):
        problems.append("has_error 与 issues 中的 error 不一致")
    # entry_id 唯一且连续
    ids = [e["entry_id"] for e in entries]
    if ids != list(range(1, len(entries) + 1)):
        problems.append("entry_id 不是从 1 开始的连续编号")
    return not problems, problems


def render_verify(data: Dict[str, Any], ok: bool, problems: List[str]) -> str:
    lines = ["=== verify 复核 ==="]
    for key, label in SUMMARY_LABELS:
        lines.append(f"{label}={data.get('summary', {}).get(key)}")
    lines.append("结果=" + ("通过" if ok else "失败"))
    for p in problems:
        lines.append(f"  - {p}")
    return "\n".join(lines)
