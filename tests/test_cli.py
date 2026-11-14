import json
from pathlib import Path

from tlcore.cli import main
from tlcore.etj_v2 import write_v2
from tlcore.model import Segment, Timeline, Track

ROOT = Path(__file__).resolve().parents[1]
FIXTURE_V2 = ROOT / "fixtures" / "ep01_v2.json"
FIXTURE_V1 = ROOT / "fixtures" / "ep01_v1.json"


def invoke(capsys, *arguments):
    code = main(list(arguments))
    output = capsys.readouterr()
    return code, output.out, output.err


def test_acceptance_flow(tmp_path, capsys):
    validate_report = tmp_path / "v.json"
    code, output, _ = invoke(
        capsys, "validate", "--in", str(FIXTURE_V2), "--report", str(validate_report)
    )
    assert code == 0
    assert "conflict_count=48" in output and "pairs=48" in output
    validated = json.loads(validate_report.read_text())
    assert validated["track_count"] == 7
    assert validated["segment_count"] == 170

    resolved_path = tmp_path / "r.v2.json"
    resolve_report = tmp_path / "r.json"
    code, output, _ = invoke(
        capsys,
        "resolve",
        "--in",
        str(FIXTURE_V2),
        "--policy",
        "ripple,trim",
        "--out",
        str(resolved_path),
        "--report",
        str(resolve_report),
    )
    assert code == 0
    assert "resolved_intervals=170" in output
    resolved = json.loads(resolve_report.read_text())
    assert resolved["unresolved_count"] == 0
    assert resolved["drift_samples"] <= 100
    assert resolved["conflict_count_after"] == 0

    schedule_report = tmp_path / "s.json"
    code, output, _ = invoke(
        capsys,
        "schedule",
        "--in",
        str(resolved_path),
        "--fps",
        "30000/1001",
        "--report",
        str(schedule_report),
    )
    assert code == 0
    schedule = json.loads(schedule_report.read_text())
    assert schedule["schedule_frames"] == 20_000
    assert len(schedule["digest"]) == 64

    converted = tmp_path / "u.v2.json"
    code, _, _ = invoke(
        capsys, "convert", "--in", str(FIXTURE_V1), "--to", "v2", "--out", str(converted)
    )
    assert code == 0 and converted.exists()

    exported = tmp_path / "e.otio"
    otio_report = tmp_path / "o.json"
    code, output, _ = invoke(
        capsys,
        "otio",
        "--in",
        str(resolved_path),
        "--export",
        str(exported),
        "--report",
        str(otio_report),
    )
    assert code == 0
    assert "roundtrip_match=true" in output
    otio_data = json.loads(otio_report.read_text())
    assert otio_data["summary"]["digest"] == resolved["summary"]["digest"]


def test_missing_input_and_invalid_model_exit_codes(tmp_path, capsys):
    code, _, _ = invoke(capsys, "validate", "--in", str(tmp_path / "missing.json"))
    assert code == 5
    bad = tmp_path / "bad.v1.json"
    bad.write_text(
        json.dumps(
            {
                "format": "etj",
                "version": 1,
                "clips": [
                    {"id": "z", "track": "v1", "start_ms": 0, "duration_ms": 0}
                ],
            }
        ),
        encoding="utf-8",
    )
    code, output, _ = invoke(capsys, "validate", "--in", str(bad))
    assert code == 2
    assert "conflict_count=1" in output


def test_locked_conflict_returns_three(tmp_path, capsys):
    source = tmp_path / "locked.v2.json"
    output = tmp_path / "locked.out.v2.json"
    timeline = Timeline(
        tracks=[
            Track(
                "v1",
                "video",
                segments=[
                    Segment("a", 0, 100, locked=True),
                    Segment("b", 20, 100),
                ],
            )
        ],
        declared_duration=120,
    )
    write_v2(timeline, source)
    code, output_text, _ = invoke(
        capsys,
        "resolve",
        "--in",
        str(source),
        "--policy",
        "drop",
        "--out",
        str(output),
    )
    assert code == 3
    assert "unresolved_count=1" in output_text


def test_drift_threshold_returns_four(tmp_path, capsys):
    source = tmp_path / "drift.v2.json"
    output = tmp_path / "drift.out.v2.json"
    timeline = Timeline(
        tracks=[Track("v1", "video", segments=[Segment("a", 0, 100), Segment("b", 50, 100)])],
        declared_duration=150,
    )
    write_v2(timeline, source)
    code, _, _ = invoke(
        capsys,
        "resolve",
        "--in",
        str(source),
        "--policy",
        "ripple",
        "--out",
        str(output),
        "--drift-threshold-samples",
        "10",
    )
    assert code == 4


def test_v2_to_v1_precision_loss_returns_four(tmp_path, capsys):
    source = tmp_path / "fine.v2.json"
    output = tmp_path / "fine.v1.json"
    timeline = Timeline(
        tracks=[Track("v1", "video", segments=[Segment("a", 24, 72)])],
        declared_duration=96,
    )
    write_v2(timeline, source)
    code, _, _ = invoke(
        capsys, "convert", "--in", str(source), "--to", "1", "--out", str(output)
    )
    assert code == 4
    assert output.exists()
