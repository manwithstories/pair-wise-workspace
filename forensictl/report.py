"""Quantitative terminal and JSON reporting from the working database."""

from __future__ import annotations

import json
from typing import TextIO

from .workdb import StoredReport, WorkStore


def _iso(value) -> str:
    return value.isoformat(timespec="microseconds").replace("+00:00", "Z")


def report_dict(report: StoredReport) -> dict:
    return {
        "events": report.events,
        "hash_mismatch": len(report.mismatches),
        "hash_mismatches": [
            {
                "block": item[0],
                "offset": item[1],
                "asset": item[2],
                "logical_offset": item[3],
                "expected_sha256": item[4],
                "actual_sha256": item[5],
                "reason": item[6],
            }
            for item in report.mismatches
        ],
        "missing_ranges": len(report.missing),
        "missing": [
            {"start": _iso(start), "end": _iso(end)} for start, end in report.missing
        ],
        "verify_issues": [
            {"kind": kind, "text": text} for kind, text in report.issues
        ],
        "warnings": report.warnings,
        "stats": report.stats,
        "exit": report.exit_code,
    }


def render_terminal(report: StoredReport) -> str:
    lines = [f"events={report.events}"]
    lines.append(f"hash_mismatch={len(report.mismatches)}")
    for block, offset, asset, logical_offset, expected, actual, reason in report.mismatches:
        lines.append(
            f"mismatch block={block} offset={offset} asset={asset} "
            f"logical_offset={logical_offset} reason={reason} "
            f"expected={expected} actual={actual}"
        )
    lines.append(f"missing_ranges={len(report.missing)}")
    for start, end in report.missing:
        lines.append(f"missing start={_iso(start)} end={_iso(end)}")
    for kind, text in report.issues:
        lines.append(f"verify_issue kind={kind} text={text}")
    for warning in report.warnings:
        lines.append(f"warning={warning}")
    lines.append(f"exit={report.exit_code}")
    return "\n".join(lines)


def print_report(report: StoredReport, stream: TextIO) -> None:
    stream.write(render_terminal(report) + "\n")


def write_json(report: StoredReport, path: str) -> None:
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(report_dict(report), handle, indent=2, sort_keys=True, ensure_ascii=False)
        handle.write("\n")


def report_from_db(path: str) -> StoredReport:
    with WorkStore(path) as store:
        return store.report()
