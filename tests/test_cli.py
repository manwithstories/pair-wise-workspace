import csv
import json
import os

import pytest

from media_probe.cli import EXIT_JUMPS, EXIT_OK, EXIT_UNDECODABLE, main


def scan_json(path, tmp_path, *extra):
    out = tmp_path / "scan.json"
    code = main(["scan", "--input", path, "--json", str(out), *extra])
    return code, json.loads(out.read_text())


def test_scan_drift_manifest(drift_path, tmp_path):
    code, manifest = scan_json(drift_path, tmp_path)
    assert code == EXIT_JUMPS  # splice gaps remain by design
    frames = manifest["frames"]
    assert frames["decoded_frames"] == pytest.approx(300, rel=0.1)
    assert frames["i_frames"] == pytest.approx(10, rel=0.3)
    assert frames["p_frames"] == pytest.approx(180, rel=0.1)
    assert frames["b_frames"] == pytest.approx(110, rel=0.1)
    assert frames["pict_type_inferred"] == 0
    timeline = manifest["timeline"]
    assert 20 <= timeline["repaired_count"] <= 40
    assert 33 <= timeline["jumps"]["max_jump_ms"] <= 120
    assert timeline["nominal_fps"] == pytest.approx(30.0)
    assert isinstance(timeline["vfr"], bool)
    assert 0.0 <= timeline["vfr_score"] <= 1.0


def test_repair_matches_scan_and_exports_csv(drift_path, tmp_path):
    scan_path = tmp_path / "scan.json"
    repair_path = tmp_path / "repair.json"
    csv_path = tmp_path / "ts.csv"
    code_scan = main(["scan", "--input", drift_path, "--json", str(scan_path)])
    code_repair = main([
        "repair", "--input", drift_path, "--jump-threshold", "1.5",
        "--json", str(repair_path), "--export", str(csv_path),
    ])
    assert code_scan == code_repair == EXIT_JUMPS
    scan = json.loads(scan_path.read_text())
    repair = json.loads(repair_path.read_text())
    assert scan["timeline"] == repair["timeline"]  # identical timeline verdict
    with open(csv_path) as handle:
        rows = list(csv.reader(handle))
    assert rows[0] == ["frame_index", "pts", "dts", "repaired"]
    assert len(rows) == 1 + scan["frames"]["decoded_frames"]
    repaired_rows = sum(1 for row in rows[1:] if row[3] == "1")
    assert repaired_rows == scan["timeline"]["repaired_count"] - scan["timeline"]["repair_events"].get("zero_shift", 0)


def test_scan_vfr_clip(fixtures_dir, tmp_path):
    code, manifest = scan_json(os.path.join(fixtures_dir, "vfr_clip.mkv"), tmp_path)
    timeline = manifest["timeline"]
    assert timeline["vfr"] is True
    assert timeline["vfr_score"] > 0.5
    assert timeline["nominal_fps"] == pytest.approx(30.3, abs=0.5)


@pytest.mark.parametrize(
    "name,expect",
    [
        ("single_frame.mp4", {"decoded": 1, "exit": EXIT_OK}),
        ("all_intra.mp4", {"decoded": 60, "exit": EXIT_OK}),
        ("same_pts.mp4", {"decoded": 60, "exit": EXIT_OK}),
        ("timebase_1k.mkv", {"decoded": 60, "exit": EXIT_OK}),
    ],
)
def test_boundary_clips(fixtures_dir, tmp_path, name, expect):
    code, manifest = scan_json(os.path.join(fixtures_dir, name), tmp_path)
    assert code == expect["exit"]
    assert manifest["frames"]["decoded_frames"] == expect["decoded"]


def test_all_intra_distribution(fixtures_dir, tmp_path):
    _, manifest = scan_json(os.path.join(fixtures_dir, "all_intra.mp4"), tmp_path)
    assert manifest["frames"]["i_frames"] == 60
    assert manifest["frames"]["b_frames"] == 0


def test_same_pts_clip_gets_monotone_timeline(fixtures_dir, tmp_path):
    _, manifest = scan_json(os.path.join(fixtures_dir, "same_pts.mp4"), tmp_path)
    events = manifest["timeline"]["repair_events"]
    assert events.get("duplicate_pts", 0) >= 50


def test_timebase_1k_reported(fixtures_dir, tmp_path):
    _, manifest = scan_json(os.path.join(fixtures_dir, "timebase_1k.mkv"), tmp_path)
    assert manifest["probe"]["time_base"] == "1/1000"


def test_truncated_clip_warns_and_continues(fixtures_dir, tmp_path):
    code, manifest = scan_json(os.path.join(fixtures_dir, "truncated.mkv"), tmp_path)
    assert code in (EXIT_OK, EXIT_JUMPS)
    assert manifest["warnings"]
    assert manifest["missing_intervals"]


def test_undecodable_exit_code(tmp_path):
    bad = tmp_path / "garbage.bin"
    bad.write_bytes(os.urandom(2048))
    assert main(["scan", "--input", str(bad)]) == EXIT_UNDECODABLE
    assert main(["scan", "--input", str(tmp_path / "missing.mp4")]) == EXIT_UNDECODABLE


def test_jump_threshold_argument(drift_path, tmp_path):
    code, manifest = scan_json(drift_path, tmp_path, "--jump-threshold", "5.0")
    assert manifest["timeline"]["jumps"]["count"] == 0
    assert code == EXIT_OK
