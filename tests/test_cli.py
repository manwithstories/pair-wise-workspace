import json
from fractions import Fraction

from tlcore.cli import main
from tlcore.model import Clip, Timeline, Track
from tlcore.tle import to_v2_document


def _json_stdout(capsys) -> dict:
    return json.loads(capsys.readouterr().out)


def test_fixture_check_render_export_flow(tmp_path, capsys):
    fixture_dir = tmp_path / "fixtures"
    assert main(["fixtures", "--seed", "20261114", "--out", str(fixture_dir)]) == 0
    generated = _json_stdout(capsys)
    assert generated["ok"] is True
    assert generated["baseline"]["conflict_count"] == 14

    report_path = tmp_path / "check.json"
    assert main([
        "check", "--input", str(fixture_dir / "show_v2.json"),
        "--policy", "ripple", "--report", str(report_path),
    ]) == 0
    checked = _json_stdout(capsys)
    assert checked["resolved"] == 14
    assert checked["unresolved"] == []
    assert checked["by_kind"] == generated["baseline"]["by_kind"]
    assert checked["duration_declared_ns"] == checked["duration_computed_ns"] == 60_000_000_000
    assert checked["deviation_ns"] == 0
    assert "video.main:[0,20000000000)" in checked["resolved_intervals"]
    assert json.loads(report_path.read_text())["iterations"] == 13

    hash_path = tmp_path / "frame_map.sha256"
    assert main([
        "render", "--input", str(fixture_dir / "show_v2.json"),
        "--policy", "ripple", "--fps", "25", "--hash", str(hash_path),
    ]) == 0
    rendered = _json_stdout(capsys)
    assert rendered["frames"] == 1500
    assert rendered["layers_max"] == 4
    assert len(rendered["sha256"]) == 64
    assert hash_path.read_text().strip() == rendered["sha256"]

    otio_path = tmp_path / "show.otio"
    assert main([
        "export", "--input", str(fixture_dir / "show_v1.json"),
        "--otio", str(otio_path),
    ]) == 0
    exported = _json_stdout(capsys)
    assert exported["otio_duration_ns"] == 60_000_000_000
    assert exported["deviation_ns"] == 0
    assert otio_path.exists()


def _write_minimal(path, *, bad_denominator: bool = False) -> None:
    start = {"num": 1, "den": 0} if bad_denominator else {"num": 0, "den": 1}
    document = {
        "version": 2,
        "fps": 25,
        "duration": {"num": 5, "den": 1},
        "tracks": [{
            "id": "video.main",
            "kind": "video",
            "clips": [{
                "id": "clip.bad",
                "start": start,
                "duration": {"num": 1, "den": 1},
            }],
        }],
    }
    path.write_text(json.dumps(document), encoding="utf-8")


def test_invalid_time_returns_exit_3_with_location(tmp_path, capsys):
    path = tmp_path / "bad.json"
    _write_minimal(path, bad_denominator=True)
    assert main(["check", "--input", str(path)]) == 3
    error = json.loads(capsys.readouterr().err)
    assert error["error"] == "TLEError"
    assert error["source"] == str(path)
    assert error["clip_id"] == "clip.bad"


def test_residual_conflict_returns_exit_2(tmp_path, capsys):
    timeline = Timeline(
        fps=Fraction(25, 1), declared_duration_ns=5_000_000_000,
        tracks=[Track("video.main", "video", [
            Clip("a", "video.main", "video", 0, 2_000_000_000,
                 source_duration_ns=2_000_000_000),
            Clip("b", "video.main", "video", 1_000_000_000, 2_000_000_000),
        ])],
    )
    timeline.validate()
    path = tmp_path / "residual.json"
    path.write_text(json.dumps(to_v2_document(timeline)), encoding="utf-8")
    assert main([
        "check", "--input", str(path), "--policy", "trim",
        "--report", str(tmp_path / "residual-report.json"),
    ]) == 2
    report = _json_stdout(capsys)
    assert report["ok"] is False
    assert report["unresolved_count"] == 1


def test_nonconvergence_and_bad_fps_return_exit_4_and_3(tmp_path, capsys):
    fixture_dir = tmp_path / "fixtures"
    assert main(["fixtures", "--out", str(fixture_dir)]) == 0
    capsys.readouterr()
    assert main([
        "check", "--input", str(fixture_dir / "show_v2.json"),
        "--max-iterations", "1",
    ]) == 4
    error = json.loads(capsys.readouterr().err)
    assert error["error"] == "NonConvergenceError"
    assert error["iterations"] == 1

    assert main([
        "render", "--input", str(fixture_dir / "show_v2.json"), "--fps", "0",
    ]) == 3
    assert json.loads(capsys.readouterr().err)["error"] == "TimebaseError"


def test_missing_file_and_bad_arguments_return_exit_3(tmp_path, capsys):
    assert main(["check", "--input", str(tmp_path / "absent.json")]) == 3
    assert json.loads(capsys.readouterr().err)["error"] == "TLEError"
    assert main(["render"]) == 3
    assert json.loads(capsys.readouterr().err)["error"] == "UsageError"
