"""Terminal tables and JSON-friendly summaries."""

from __future__ import annotations

import json
from typing import Any, Iterable


def json_text(value: Any, *, compact: bool = False) -> str:
    if compact:
        return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False)


def table(headers: list[str], rows: Iterable[Iterable[Any]]) -> str:
    materialized = [[str(value) for value in row] for row in rows]
    widths = [len(header) for header in headers]
    for row in materialized:
        for index, value in enumerate(row):
            widths[index] = max(widths[index], len(value))
    rule = "+" + "+".join("-" * (width + 2) for width in widths) + "+"
    lines = [rule, "| " + " | ".join(header.ljust(widths[index]) for index, header in enumerate(headers)) + " |", rule]
    for row in materialized:
        lines.append("| " + " | ".join(value.ljust(widths[index]) for index, value in enumerate(row)) + " |")
    lines.append(rule)
    return "\n".join(lines)


def scan_summary(stats: dict[str, Any]) -> dict[str, Any]:
    return {
        "skipped": stats.get("skipped", 0),
        "completed": stats.get("completed", 0),
        "done": stats.get("done", 0),
        "failed": stats.get("failed", 0),
        "pending": stats.get("pending", 0),
        "attempts": stats.get("attempts", 0),
        "recovered": stats.get("recovered", 0),
        "cleaned_temps": stats.get("cleaned_temps", 0),
        "interrupted": bool(stats.get("interrupted", False)),
        "elapsed_sec": stats.get("elapsed_sec", 0.0),
    }


def format_scan(stats: dict[str, Any]) -> str:
    summary = scan_summary(stats)
    lines = [
        table(["metric", "value"], [
            ("done", summary["done"]),
            ("skipped", summary["skipped"]),
            ("completed", summary["completed"]),
            ("failed", summary["failed"]),
            ("attempts", summary["attempts"]),
            ("recovered", summary["recovered"]),
            ("cleaned_temps", summary["cleaned_temps"]),
            ("interrupted", str(summary["interrupted"]).lower()),
            ("elapsed_sec", f"{summary['elapsed_sec']:.3f}"),
        ])
    ]
    if summary["interrupted"]:
        lines.append("scan interrupted; rerun with --resume")
    return "\n".join(lines)


def format_ladder(ladder: dict[str, Any]) -> str:
    rows = [
        (
            index + 1,
            level.get("resolution", ""),
            f"{float(level.get('bitrate_kbps', 0)):.1f}",
            f"{float(level.get('ssim', 0)):.4f}",
            f"{float(level.get('psnr_db', 0)):.2f}",
            str(level.get("monotone", False)).lower(),
            level.get("name", ""),
        )
        for index, level in enumerate(ladder.get("levels", []))
    ]
    body = table(["rung", "resolution", "kbps", "ssim", "psnr_db", "monotone", "name"], rows)
    return body + f"\nlevels={len(rows)} dropped={len(ladder.get('dropped', []))} monotone={str(ladder.get('monotone')).lower()}"


def format_verify(result: dict[str, Any]) -> str:
    rows = [
        (index + 1, violation.get("level", ""), violation.get("code", ""), violation.get("task_key", ""))
        for index, violation in enumerate(result.get("violations", []))
    ]
    body = table(["#", "level", "code", "task_key"], rows) if rows else "violations: none"
    return body + f"\npass={str(result.get('pass')).lower()} monotone={str(result.get('monotone')).lower()} checked={result.get('checked', 0)}"
