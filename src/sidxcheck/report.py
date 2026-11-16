"""Stable JSON report schema and terminal summaries."""

from __future__ import annotations

import json
from pathlib import Path
from typing import TextIO

from .rebuild import RebuildResult
from .verify import FatalRecord, OffsetMismatch, ScanResult, VerificationResult


def build_report(
    scan: ScanResult,
    *,
    verification: VerificationResult | None = None,
    rebuilt: RebuildResult | None = None,
) -> dict[str, object]:
    offset_mismatch = verification.offset_mismatch if verification is not None else 0
    max_offset = (
        max(verification.offset_mismatches, key=lambda item: item.max_abs_delta, default=None)
        if verification is not None
        else None
    )
    drift = (
        rebuilt.max_drift_ticks
        if rebuilt is not None
        else verification.max_drift_ticks if verification is not None else 0
    )
    payload: dict[str, object] = {
        "sample_count": scan.sample_count,
        "sidx_entries": scan.sidx_entries,
        "boundary_count": scan.boundary_count,
        "offset_mismatch": offset_mismatch,
        "offset_max_drift": max_offset.max_abs_delta if max_offset is not None else 0,
        "offset_max_drift_index": max_offset.entry_index if max_offset is not None else None,
        "max_drift_ticks": drift,
        "duration_ms": scan.duration_ms,
        "fatal_count": scan.fatal_count,
        "files": [str(path) for path in scan.files],
        "sidx_hierarchical_entries": scan.sidx_hierarchical_entries,
        "offset_mismatches": (
            [_offset_dict(mismatch) for mismatch in verification.offset_mismatches]
            if verification is not None
            else []
        ),
        "validation_errors": verification.validation_errors if verification is not None else [],
        "fatal": [_fatal_dict(record) for record in scan.fatal],
    }
    if rebuilt is not None:
        payload.update(
            {
                "rebuilt_boundaries": rebuilt.boundary_count,
                "first_offset": rebuilt.first_offset,
                "earliest_presentation_time": rebuilt.earliest_presentation_time,
                "rebuilt_duration_ticks": rebuilt.duration_ticks,
                "rebuild_issues": rebuilt.issues,
            }
        )
    return payload


def _offset_dict(mismatch: OffsetMismatch) -> dict[str, object]:
    return {
        "file": str(mismatch.file), "index": mismatch.entry_index,
        "declared_start": mismatch.declared_start, "actual_start": mismatch.actual_start,
        "start_delta": mismatch.start_delta, "declared_end": mismatch.declared_end,
        "actual_end": mismatch.actual_end, "end_delta": mismatch.end_delta,
    }


def _fatal_dict(record: FatalRecord) -> dict[str, object]:
    return {
        "file": str(record.file), "offset": record.offset,
        "code": record.code, "message": record.message,
    }


def write_json(path: str | Path, payload: dict[str, object]) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def print_scan_summary(scan: ScanResult, stream: TextIO) -> None:
    print(
        f"sidx_entries={scan.sidx_entries} boundary_count={scan.boundary_count} "
        f"sample_count={scan.sample_count} duration_ms={scan.duration_ms}",
        file=stream,
    )


def print_verify_summary(verification: VerificationResult, stream: TextIO) -> None:
    scan = verification.scan
    print_scan_summary(scan, stream)
    print(
        f"offset_mismatch={verification.offset_mismatch} "
        f"offset_max_drift={verification.offset_max_drift} "
        f"max_drift_ticks={verification.max_drift_ticks} fatal_count={scan.fatal_count}",
        file=stream,
    )
    for mismatch in verification.offset_mismatches:
        print(
            f"offset_mismatch index={mismatch.entry_index} "
            f"start_delta={mismatch.start_delta} end_delta={mismatch.end_delta} "
            f"file={mismatch.file}",
            file=stream,
        )
    for record in scan.fatal:
        print(
            f"fatal file={record.file} offset={record.offset} "
            f"code={record.code} message={record.message}",
            file=stream,
        )
    for error in verification.validation_errors:
        print(f"validation_error {error}", file=stream)


def print_rebuild_summary(rebuilt: RebuildResult, stream: TextIO) -> None:
    print(
        f"rebuilt_boundaries={rebuilt.boundary_count} "
        f"max_drift_ticks={rebuilt.max_drift_ticks}",
        file=stream,
    )
    for issue in rebuilt.issues:
        print(f"rebuild_error {issue}", file=stream)
