"""Terminal table rendering and JSON summary construction."""
from __future__ import annotations

import json

from .validate import FileReport


def report_to_dict(report: FileReport) -> dict:
    """Flat JSON-serializable summary for one file."""
    check = report.offset_check
    comp = report.comparison
    return {
        "file": report.path,
        "sample_count": report.sample_count,
        "sidx_entries": report.sidx_entries,
        "offset_check": {
            "status": check.status,
            "in_range": check.in_range,
            "total": check.total,
            "violations": [
                {
                    "sample_index": s.index,
                    "offset": s.offset,
                    "size": s.size,
                }
                for s in check.violations
            ],
        },
        "rebuild_boundaries": [hex(b) for b in report.rebuild_boundaries],
        "entry_count_delta": comp.entry_count_delta,
        "offset_delta_total": comp.offset_delta_total,
        "duration_delta_total": comp.duration_delta_total,
        "first_mismatch": comp.first_mismatch,
        "verdict": report.verdict,
    }


def write_json(report: FileReport, path) -> None:
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(report_to_dict(report), fh, indent=2, ensure_ascii=False)
        fh.write("\n")


def format_report(report: FileReport) -> str:
    """Human-readable per-file block for the terminal."""
    data = report_to_dict(report)
    check = report.offset_check
    lines = [
        f"file={data['file']}",
        f"  sample_count={report.sample_count}"
        f"  sidx_entries={report.sidx_entries}",
        f"  offset_check={check.status}"
        f" ({check.in_range}/{check.total} samples within mdat bounds)",
        "  rebuild_boundaries="
        + "[" + ", ".join(data["rebuild_boundaries"]) + "]",
        f"  duration_delta_total={report.comparison.duration_delta_total}"
        f"  offset_delta_total={report.comparison.offset_delta_total}"
        f"  entry_count_delta={report.comparison.entry_count_delta}",
    ]
    if report.comparison.first_mismatch:
        lines.append(f"  first_mismatch={report.comparison.first_mismatch}")
    if check.violations:
        listed = ", ".join(str(s.index) for s in check.violations)
        lines.append(
            f"  offset_violations={check.violation_count} (first: {listed})")
    lines.append(f"  verdict={report.verdict}")
    return "\n".join(lines)


def format_summary(reports: list[FileReport],
                   errors: list[tuple[str, str]] | None = None) -> str:
    """Summary table for batch runs; errors are (path, message) pairs."""
    header = f"{'FILE':<40} {'SAMPLES':>7} {'SIDX':>5} {'OFFSET':>6} VERDICT"
    lines = [header, "-" * len(header)]
    for report in reports:
        name = report.path
        if len(name) > 38:
            name = "..." + name[-35:]
        lines.append(
            f"{name:<40} {report.sample_count:>7} {report.sidx_entries:>5}"
            f" {report.offset_check.status:>6} {report.verdict}")
    for path, _message in (errors or []):
        name = str(path)
        if len(name) > 38:
            name = "..." + name[-35:]
        lines.append(f"{name:<40} {'-':>7} {'-':>5} {'-':>6} ERROR")
    total = len(reports) + len(errors or [])
    passed = sum(1 for r in reports if r.verdict == "PASS")
    lines.append(f"{passed}/{total} files PASS")
    return "\n".join(lines)
