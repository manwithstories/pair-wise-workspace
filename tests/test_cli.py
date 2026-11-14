from __future__ import annotations

import json
from pathlib import Path

from cutplan.cli import main
from cutplan.convert import save_timeline


def output_json(capsys) -> dict:
    captured = capsys.readouterr()
    return json.loads(captured.out)


def prepare_plans(conflict_timeline, tmp_path: Path) -> tuple[Path, Path]:
    v1 = tmp_path / "conflicts_v1.txt"
    v2 = tmp_path / "conflicts_v2.json"
    save_timeline(conflict_timeline, v1, "v1")
    save_timeline(conflict_timeline, v2, "v2")
    return v1, v2


def test_cli_acceptance_sequence(conflict_timeline, tmp_path: Path, capsys) -> None:
    v1, v2 = prepare_plans(conflict_timeline, tmp_path)
    assert main(["validate", str(v1), "--json"]) == 3
    report = output_json(capsys)
    assert report["tracks"] == 5
    assert report["clips"] == 42
    assert report["conflicts"]["clusters"] == 3
    assert report["conflicts"]["pairs"] == 5

    resolved = tmp_path / "resolved_v2.json"
    report_path = tmp_path / "report.json"
    assert main(
        [
            "resolve",
            str(v2),
            "--policy",
            "trim_head",
            "--out",
            str(resolved),
            "--report",
            str(report_path),
        ]
    ) == 0
    resolution = output_json(capsys)
    assert resolution["resolved_clips"] == 39
    assert resolution["removed_zero"] == 2
    assert resolution["ripple_shifted"] == 7
    assert resolution["clusters_after"] == 0
    assert resolution["duration_delta_frames"] == 0
    assert resolution["duration_delta_seconds"] == 0.0
    assert report_path.exists()

    migrated = tmp_path / "migrated.json"
    assert main(["convert", str(v1), "--to", "v2", "--out", str(migrated)]) == 0
    assert output_json(capsys)["version"] == "v2"

    otio_path = tmp_path / "resolved.otio"
    assert main(["otio", "export", str(resolved), "--out", str(otio_path)]) == 0
    output_json(capsys)
    assert main(["otio", "roundtrip", str(otio_path), "--json"]) == 0
    roundtrip = output_json(capsys)
    assert roundtrip["equal"]
    assert roundtrip["duration_delta_frames"] == 0

    summaries = []
    for index in (1, 2):
        frame_dir = tmp_path / f"frames-{index}"
        assert main(
            [
                "render",
                str(resolved),
                "--out-dir",
                str(frame_dir),
                "--width",
                "64",
                "--height",
                "36",
                "--json",
            ]
        ) == 0
        summaries.append(output_json(capsys))
    assert summaries[0]["frame_count"] == 300
    assert summaries[0]["gap_frames"] == 12
    assert summaries[0]["first_hash"] == summaries[1]["first_hash"]
    assert summaries[0]["digest"] == summaries[1]["digest"]


def test_cli_unknown_version_and_unresolved_render(conflict_timeline, tmp_path: Path, capsys) -> None:
    _, v2 = prepare_plans(conflict_timeline, tmp_path)
    bad = tmp_path / "bad.json"
    bad.write_text('{"schema":"cutplan/v9","version":9}', encoding="utf-8")
    assert main(["validate", str(bad), "--json"]) == 2
    error = output_json(capsys)["error"]
    assert error["code"] == "E_FORMAT_VERSION"
    assert "supported: v1, v2" in error["message"]
    assert main(["render", str(v2), "--out-dir", str(tmp_path / "frames"), "--json"]) == 4
    assert output_json(capsys)["error"]["code"] == "E_UNRESOLVED_CONFLICT"


def test_cli_batch_continues_after_failure_and_reports_codes(conflict_timeline, tmp_path: Path, capsys) -> None:
    directory = tmp_path / "batch"
    directory.mkdir()
    save_timeline(conflict_timeline, directory / "good.json", "v2")
    (directory / "bad.json").write_text("CUTPLAN|9\nEND\n", encoding="utf-8")
    assert main(["validate", str(directory), "--json", "--batch"]) == 3
    summary = output_json(capsys)
    assert summary["success"] == 1
    assert summary["failed"] == 1
    failed = [item for item in summary["files"] if not item["ok"]]
    assert failed[0]["error_code"] == "E_FORMAT_VERSION"
    assert "message" not in failed[0]


def test_cli_resolve_batch_writes_outputs(conflict_timeline, tmp_path: Path, capsys) -> None:
    directory = tmp_path / "batch"
    directory.mkdir()
    save_timeline(conflict_timeline, directory / "one.json", "v2")
    output_dir = tmp_path / "resolved"
    assert main(
        [
            "resolve",
            str(directory),
            "--policy",
            "trim_head",
            "--out-dir",
            str(output_dir),
            "--json",
        ]
    ) == 0
    summary = output_json(capsys)
    assert summary["success"] == 1
    assert (output_dir / "one.resolved.json").exists()
    assert (output_dir / "one.report.json").exists()
