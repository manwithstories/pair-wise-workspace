import json
import os
import subprocess
import sys

import pytest

from mkvtool.fixtures import FixtureOptions, build_fixture

ROOT = os.path.dirname(os.path.dirname(__file__))


def run_cli(*args):
    return subprocess.run(
        [sys.executable, "-m", "mkvtool", *args],
        cwd=ROOT,
        text=True,
        capture_output=True,
        timeout=20,
    )


@pytest.fixture(scope="module")
def acceptance(tmp_path_factory):
    directory = tmp_path_factory.mktemp("acceptance")
    fixtures = directory / "fixtures"
    output = directory / "out"
    generated = run_cli("gen-fixtures", "--out", str(fixtures), "--cases", "7")
    assert generated.returncode == 0, generated.stderr
    probed = run_cli(
        "probe", str(fixtures / "unknown_segment.mkv"),
        "--json", str(output / "tree.json"),
    )
    assert probed.returncode == 0, probed.stderr
    remuxed = run_cli(
        "remux", str(fixtures / "negative_dts_b.mkv"),
        "-o", str(output / "neg.mp4"),
        "--report", str(output / "neg.report.json"),
    )
    assert remuxed.returncode == 0, remuxed.stderr
    verified = run_cli(
        "verify", str(output / "neg.mp4"),
        "--report", str(output / "neg.report.json"),
    )
    assert verified.returncode == 0, verified.stderr
    return {
        "dir": directory,
        "fixtures": fixtures,
        "output": output,
        "probe": json.loads(probed.stdout),
        "remux": json.loads(remuxed.stdout),
        "verify": json.loads(verified.stdout),
    }


def test_acceptance_metrics(acceptance):
    probe = acceptance["probe"]
    assert 4100 <= probe["element_count"] <= 4400
    assert probe["unknown_size_elements"] == 9
    assert probe["max_depth"] == 6
    remux = acceptance["remux"]
    assert remux["tracks"] == 2
    assert remux["video_frames"] == 1500
    assert remux["first_dts_ms"] == -80.0
    assert remux["rewritten_first_dts_ms"] == 0.0
    assert remux["keyframes"] == 38
    verify = acceptance["verify"]
    assert verify["frame_count_match"] is True
    assert verify["dts_monotonic"] is True
    assert verify["index_matches"] is True
    assert verify["timestamps_match"] is True


def test_probe_json_is_shared_parse_output(acceptance):
    payload = json.loads((acceptance["output"] / "tree.json").read_text())
    assert payload["element_count"] == acceptance["probe"]["element_count"]
    assert payload["track_map"]["track_count"] == 2
    assert len(payload["unknown_size_paths"]) == 9


def test_corrupt_header_exit_code_two(acceptance):
    result = run_cli("probe", str(acceptance["fixtures"] / "corrupt_header.mkv"))
    assert result.returncode == 2
    assert "EBML" in result.stderr


def test_truncated_input_degrades_successfully(acceptance):
    result = run_cli("probe", str(acceptance["fixtures"] / "truncated_cluster.mkv"))
    assert result.returncode == 0
    payload = json.loads(result.stdout)
    assert payload["truncated"] is True
    assert payload["element_count"] > 0


def test_negative_timestamp_without_shift_exit_code_four(acceptance):
    result = run_cli(
        "remux", str(acceptance["fixtures"] / "negative_dts_b.mkv"),
        "-o", str(acceptance["output"] / "no-shift.mp4"), "--no-shift",
    )
    assert result.returncode == 4
    assert "negative DTS" in result.stderr


def test_unsupported_only_track_exit_code_three(tmp_path):
    fixture = build_fixture(FixtureOptions(
        "unsupported.mkv", video_frames=2, audio_frames=2,
        cluster_count=1, all_unsupported=True,
    ))
    source = tmp_path / "unsupported.mkv"
    source.write_bytes(fixture.data)
    result = run_cli("remux", str(source), "-o", str(tmp_path / "bad.mp4"))
    assert result.returncode == 3


def test_verify_mismatch_exit_code_five(acceptance, tmp_path):
    report = json.loads((acceptance["output"] / "neg.report.json").read_text())
    report["video_frames"] += 1
    tampered = tmp_path / "tampered.report.json"
    tampered.write_text(json.dumps(report))
    result = run_cli(
        "verify", str(acceptance["output"] / "neg.mp4"), "--report", str(tampered)
    )
    assert result.returncode == 5
