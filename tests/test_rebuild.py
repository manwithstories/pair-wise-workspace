from __future__ import annotations

from pathlib import Path

import pytest

from sidxcheck.boxes import parse_bytes, parse_file
from sidxcheck.cli import main
from sidxcheck.fixtures import SEGMENT_SECONDS, TIMESCALE, generate_fixture
from sidxcheck.rebuild import rebuild_sidx
from sidxcheck.report import build_report, write_json
from sidxcheck.sidx import encode_sidx, parse_sidx
from sidxcheck.verify import exit_code, scan_path, verify_scan

FIXED_KEYS = {
    "sample_count",
    "sidx_entries",
    "boundary_count",
    "offset_mismatch",
    "max_drift_ticks",
    "duration_ms",
    "fatal_count",
}


def test_normal_scan_and_rebuild_are_consistent(tmp_path: Path) -> None:
    target = tmp_path / "normal"
    generate_fixture(target, segments=12, writer="normal")
    scan = scan_path(target)
    assert scan.sidx_entries == 12
    assert scan.boundary_count == 12
    assert scan.sample_count == 3840
    assert scan.duration_ms == 48_000
    verification = verify_scan(scan)
    assert exit_code(verification) == 0
    assert verification.offset_mismatch == 0
    assert verification.max_drift_ticks == 0

    rebuilt = rebuild_sidx(scan.moofs, sidxes=scan.sidxes, sample_tables=scan.sample_tables)
    assert rebuilt.boundary_count == 12
    assert rebuilt.max_drift_ticks == 0
    boxes, issues = parse_bytes(rebuilt.data)
    assert not issues
    parsed = parse_sidx(boxes[0], rebuilt.data)
    assert len(parsed.entries) == 12
    assert parsed.earliest_presentation_time == 0
    assert parsed.total_duration == 12 * SEGMENT_SECONDS * TIMESCALE

    report = build_report(scan, verification=verification, rebuilt=rebuilt)
    assert FIXED_KEYS <= report.keys()
    assert report["sample_count"] == 3840
    assert report["offset_mismatch"] == 0
    assert report["offset_max_drift"] == 0
    assert report["fatal_count"] == 0
    assert report["rebuilt_boundaries"] == 12
    output = tmp_path / "report.json"
    write_json(output, report)
    assert output.read_text(encoding="utf-8").startswith("{\n")


def test_missing_index_is_rebuilt(tmp_path: Path) -> None:
    target = tmp_path / "missing"
    generate_fixture(target, segments=12, writer="missing")
    scan = scan_path(target)
    assert scan.sidx_entries == 0
    assert scan.boundary_count == 12
    assert verify_scan(scan).inconsistent
    rebuilt = rebuild_sidx(scan.moofs, sidxes=scan.sidxes, sample_tables=scan.sample_tables)
    assert rebuilt.boundary_count == 12
    assert rebuilt.max_drift_ticks == 0
    boxes, _ = parse_bytes(rebuilt.data)
    parsed = parse_sidx(boxes[0], rebuilt.data)
    assert len(parsed.entries) == 12


def test_bad_reference_count_triggers_rebuild(tmp_path: Path) -> None:
    target = tmp_path / "bad_counts"
    generate_fixture(target, segments=12, writer="bad_counts")
    scan = scan_path(target)
    assert scan.sidx_entries == 11
    assert scan.boundary_count == 12
    result = verify_scan(scan)
    assert result.inconsistent
    assert any("reference_count=11" in error for error in result.validation_errors)
    rebuilt = rebuild_sidx(scan.moofs, sidxes=scan.sidxes, sample_tables=scan.sample_tables)
    assert rebuilt.boundary_count == 12
    boxes, _ = parse_bytes(rebuilt.data)
    parsed = parse_sidx(boxes[0], rebuilt.data)
    assert parsed.declared_reference_count == 12


def test_mixed_writer_reports_two_offsets_and_one_fatal(tmp_path: Path) -> None:
    target = tmp_path / "mixed"
    generate_fixture(target, segments=12, writer="mixed")
    scan = scan_path(target)
    assert (scan.sidx_entries, scan.boundary_count) == (12, 12)
    assert (scan.sample_count, scan.duration_ms) == (3840, 48_000)
    assert scan.fatal_count == 1
    result = verify_scan(scan)
    assert result.offset_mismatch == 2
    assert result.offset_max_drift == 128
    assert [item.entry_index for item in result.offset_mismatches] == [5, 6]
    assert [item.end_delta for item in result.offset_mismatches] == [-128, 0]
    assert [item.start_delta for item in result.offset_mismatches] == [0, -128]
    assert result.max_drift_ticks == 400
    assert exit_code(result) == 3

    rebuilt = rebuild_sidx(scan.moofs, sidxes=scan.sidxes, sample_tables=scan.sample_tables)
    assert rebuilt.boundary_count == 12
    assert rebuilt.max_drift_ticks == 0
    assert rebuilt.earliest_presentation_time == 0
    boxes, _ = parse_bytes(rebuilt.data)
    parsed = parse_sidx(boxes[0], rebuilt.data)
    assert len(parsed.entries) == 12
    assert parsed.earliest_presentation_time == 0


def test_first_offset_out_of_bounds_is_reported(tmp_path: Path) -> None:
    target = tmp_path / "offset"
    media = generate_fixture(target, segments=3, writer="normal")
    raw = media.read_bytes()
    with parse_file(media) as parsed:
        sidx_box = next(box for box in parsed.boxes if box.type == "sidx")
        old = parse_sidx(sidx_box, parsed.data)
    replacement = encode_sidx(
        reference_id=old.reference_id,
        timescale=old.timescale,
        earliest_presentation_time=old.earliest_presentation_time,
        first_offset=10**9,
        entries=old.entries,
    )
    assert len(replacement) == sidx_box.size
    media.write_bytes(raw[:sidx_box.start] + replacement + raw[sidx_box.end:])
    result = verify_scan(scan_path(target))
    assert any("first_offset" in error for error in result.validation_errors)
    assert exit_code(result) == 1


def test_cli_acceptance_paths(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    target = tmp_path / "mixed"
    assert main(["gen", "--out", str(target), "--segments", "12", "--writer", "mixed"]) == 0
    capsys.readouterr()
    assert main(["scan", str(target)]) == 3
    scan_output = capsys.readouterr().out
    assert "sidx_entries=12 boundary_count=12 sample_count=3840 duration_ms=48000" in scan_output

    rebuilt_path = tmp_path / "rebuilt.sidx"
    report_path = tmp_path / "rebuilt.json"
    assert main(["rebuild", str(target), "--out", str(rebuilt_path), "--report", str(report_path)]) == 3
    rebuild_output = capsys.readouterr().out
    assert "rebuilt_boundaries=12 max_drift_ticks=0" in rebuild_output
    assert rebuilt_path.read_bytes()
    assert report_path.exists()

    verify_path = tmp_path / "verify.json"
    assert main(["verify", str(target), "--json", str(verify_path)]) == 3
    verify_output = capsys.readouterr().out
    assert "offset_mismatch=2" in verify_output
    assert "fatal_count=1" in verify_output
    assert "index=5" in verify_output and "index=6" in verify_output


def test_cli_io_error_returns_two(tmp_path: Path) -> None:
    assert main(["scan", str(tmp_path / "does-not-exist")]) == 2
