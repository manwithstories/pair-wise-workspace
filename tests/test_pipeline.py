"""End-to-end pipeline tests: fixtures -> report -> normalize -> selfcheck."""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

from plateqc import cli
from plateqc.fixtures import make_samples

REPO_ROOT = Path(__file__).resolve().parent.parent


@pytest.fixture(scope="session")
def samples(tmp_path_factory):
    out_dir = tmp_path_factory.mktemp("samples")
    make_samples.main(["--out", str(out_dir)])
    return out_dir


@pytest.fixture(scope="session")
def broken_report(samples, tmp_path_factory):
    json_path = tmp_path_factory.mktemp("out") / "report.json"
    rc = cli.main(
        [
            "report",
            "--input",
            str(samples / "plate_bframes_broken.mp4"),
            "--json",
            str(json_path),
        ]
    )
    assert rc == cli.EXIT_OK
    return json.loads(json_path.read_text())


def test_report_broken_expected_fields(broken_report):
    assert broken_report["frames_decoded"] == 600
    assert broken_report["i_count"] == 6
    assert broken_report["p_count"] == 194
    assert broken_report["b_count"] == 400
    assert broken_report["ts_repaired"] == 37
    assert broken_report["max_jump_ms"] == 842.5
    assert broken_report["duration_ms"] == 20000
    assert broken_report["warnings"] == ["timestamp_regression", "gap"]


def test_normalize_matches_report(samples, broken_report, tmp_path):
    output = tmp_path / "plate_fixed.mp4"
    json_path = tmp_path / "normalize.json"
    rc = cli.main(
        [
            "normalize",
            "--input",
            str(samples / "plate_bframes_broken.mp4"),
            "--output",
            str(output),
            "--json",
            str(json_path),
        ]
    )
    assert rc == cli.EXIT_OK
    data = json.loads(json_path.read_text())
    assert data["frames"] == 600
    assert data["duration_ms"] == 20000
    assert data["first_pts_ms"] == 0
    assert data["last_pts_ms"] == 19967
    # Same FrameTimeline -> byte-identical values on both output paths.
    assert data["ts_repaired"] == broken_report["ts_repaired"]
    assert data["max_jump_ms"] == broken_report["max_jump_ms"]


def test_fixed_file_is_clean(samples, tmp_path):
    output = tmp_path / "plate_fixed.mp4"
    json_path = tmp_path / "fixed_report.json"
    assert (
        cli.main(
            [
                "normalize",
                "--input",
                str(samples / "plate_bframes_broken.mp4"),
                "--output",
                str(output),
            ]
        )
        == cli.EXIT_OK
    )
    rc = cli.main(["report", "--input", str(output), "--json", str(json_path)])
    assert rc == cli.EXIT_OK
    data = json.loads(json_path.read_text())
    assert data["ts_repaired"] == 0
    assert data["max_jump_ms"] == 0
    assert data["frames_decoded"] == 600


def test_selfcheck_passes(samples, tmp_path):
    rc = cli.main(
        [
            "selfcheck",
            "--input",
            str(samples / "plate_bframes_broken.mp4"),
            "--work",
            str(tmp_path / "sc"),
        ]
    )
    assert rc == cli.EXIT_OK


def test_normal_sample_clean(samples, tmp_path):
    json_path = tmp_path / "normal.json"
    rc = cli.main(
        [
            "report",
            "--input",
            str(samples / "plate_bframes_normal.mp4"),
            "--json",
            str(json_path),
        ]
    )
    assert rc == cli.EXIT_OK
    data = json.loads(json_path.read_text())
    assert data["ts_repaired"] == 0
    assert data["b_count"] > 0
    assert data["warnings"] == []


def test_truncated_sample_warns(samples, tmp_path):
    json_path = tmp_path / "truncated.json"
    rc = cli.main(
        [
            "report",
            "--input",
            str(samples / "plate_truncated.mp4"),
            "--json",
            str(json_path),
        ]
    )
    assert rc == cli.EXIT_OK
    data = json.loads(json_path.read_text())
    assert "truncated_tail" in data["warnings"]
    assert data["frames_decoded"] < 400


def test_all_i_sample(samples, tmp_path):
    json_path = tmp_path / "alli.json"
    rc = cli.main(
        [
            "report",
            "--input",
            str(samples / "plate_all_i.mp4"),
            "--json",
            str(json_path),
        ]
    )
    assert rc == cli.EXIT_OK
    data = json.loads(json_path.read_text())
    assert data["i_count"] == data["frames_decoded"]
    assert data["b_count"] == 0


def test_missing_input_exit_3():
    rc = cli.main(["report", "--input", "does_not_exist.mp4"])
    assert rc == cli.EXIT_UNDECODABLE


def test_undecodable_input_exit_3(tmp_path):
    junk = tmp_path / "junk.mp4"
    junk.write_bytes(b"this is not a media file" * 16)
    rc = cli.main(["report", "--input", str(junk)])
    assert rc == cli.EXIT_UNDECODABLE


def test_bad_arguments_exit_2():
    with pytest.raises(SystemExit) as excinfo:
        cli.main(["report"])  # missing --input
    assert excinfo.value.code == cli.EXIT_ARGS


def test_module_entrypoint(samples, tmp_path):
    json_path = tmp_path / "module_report.json"
    proc = subprocess.run(
        [
            sys.executable,
            "-m",
            "plateqc.cli",
            "report",
            "--input",
            str(samples / "plate_bframes_broken.mp4"),
            "--json",
            str(json_path),
        ],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
    )
    assert proc.returncode == 0, proc.stderr
    data = json.loads(json_path.read_text())
    assert data["ts_repaired"] == 37
