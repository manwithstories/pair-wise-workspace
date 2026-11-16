"""CLI tests: exit codes, workspace confinement, reports, offline verify."""

from __future__ import annotations

import json
import shutil
from pathlib import Path

import pytest

from audio_tool import cli


def run(argv: list[str]) -> int:
    return cli.main(argv)


def copy_fixtures(fixture_tree, destination: Path, names) -> Path:
    destination.mkdir(parents=True, exist_ok=True)
    for name in names:
        shutil.copy(fixture_tree[name], destination / f"{name}.wav")
    return destination


def test_batch_exit_zero_when_all_succeed(fixture_tree, tmp_path):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in",
                           ["tone_44100_stereo", "quiet_music"])
    code = run(["batch", str(inputs), "--output", str(tmp_path / "out"),
                "--workspace", str(tmp_path), "--quiet"])
    assert code == 0
    assert len(list((tmp_path / "out").glob("*.wav"))) == 2


def test_batch_exit_one_on_partial_failure(fixture_tree, tmp_path):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in",
                           ["tone_44100_stereo", "broken_bad_channels"])
    code = run(["batch", str(inputs), "--output", str(tmp_path / "out"),
                "--workspace", str(tmp_path), "--quiet"])
    assert code == 1
    outputs = list((tmp_path / "out").glob("*.wav"))
    assert [p.name for p in outputs] == ["tone_44100_stereo.wav"]


def test_batch_exit_zero_when_only_failure_is_silence(fixture_tree, tmp_path):
    # Silence is a valid delivery file with an undefined loudness target: it
    # passes through at unity and earns a status note rather than a failure.
    inputs = copy_fixtures(fixture_tree, tmp_path / "in",
                           ["tone_44100_stereo", "silent"])
    report_path = tmp_path / "run.json"
    code = run(["batch", str(inputs), "--output", str(tmp_path / "out"),
                "--report", str(report_path), "--workspace", str(tmp_path),
                "--quiet"])
    assert code == 0
    document = json.loads(report_path.read_text())
    assert document["summary"]["silent"] == 1
    silent = next(e for e in document["files"] if e["source"] == "silent.wav")
    assert silent["stats"]["status"] == "silent"
    assert silent["stats"]["gain_db"] == 0.0


def test_batch_exit_one_when_every_file_fails(fixture_tree, tmp_path):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in", ["broken_bad_rate"])
    code = run(["batch", str(inputs), "--output", str(tmp_path / "out"),
                "--workspace", str(tmp_path), "--quiet"])
    assert code == 1


def test_batch_exit_two_on_missing_input(tmp_path):
    code = run(["batch", str(tmp_path / "does_not_exist"),
                "--output", str(tmp_path / "out"),
                "--workspace", str(tmp_path), "--quiet"])
    assert code == 2


def test_batch_exit_two_on_bad_sample_rate(fixture_tree, tmp_path):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in", ["tone_44100_stereo"])
    code = run(["batch", str(inputs), "--output", str(tmp_path / "out"),
                "--sample-rate", "4000", "--workspace", str(tmp_path), "--quiet"])
    assert code == 2


def test_batch_exit_two_on_bad_true_peak(fixture_tree, tmp_path):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in", ["tone_44100_stereo"])
    code = run(["batch", str(inputs), "--output", str(tmp_path / "out"),
                "--true-peak", "5", "--workspace", str(tmp_path), "--quiet"])
    assert code == 2


def test_batch_exit_two_when_output_outside_workspace(fixture_tree, tmp_path):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in", ["tone_44100_stereo"])
    code = run(["batch", str(inputs), "--output", str(tmp_path.parent / "elsewhere"),
                "--workspace", str(tmp_path), "--quiet"])
    assert code == 2


def test_batch_refuses_input_inside_output(fixture_tree, tmp_path):
    out = copy_fixtures(fixture_tree, tmp_path / "out", ["tone_44100_stereo"])
    code = run(["batch", str(out), "--output", str(out),
                "--workspace", str(tmp_path), "--quiet"])
    assert code == 2


def test_batch_does_not_modify_inputs(fixture_tree, tmp_path):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in", ["tone_44100_stereo"])
    before = (inputs / "tone_44100_stereo.wav").read_bytes()
    run(["batch", str(inputs), "--output", str(tmp_path / "out"),
         "--workspace", str(tmp_path), "--quiet"])
    assert (inputs / "tone_44100_stereo.wav").read_bytes() == before


def test_report_is_written_and_reloadable(fixture_tree, tmp_path):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in",
                           ["tone_44100_stereo", "tone_48000_51"])
    report_path = tmp_path / "reports" / "run.json"
    code = run(["batch", str(inputs), "--output", str(tmp_path / "out"),
                "--report", str(report_path), "--workspace", str(tmp_path),
                "--target-lufs", "-16", "--sample-rate", "48000", "--quiet"])
    assert code == 0
    document = json.loads(report_path.read_text())
    assert document["schema"] == "audio-tool/report/1"
    assert document["algorithm_version"].startswith("bs1770-4")
    assert document["config"]["target_rate"] == 48000
    assert document["config"]["target_lufs"] == -16.0
    assert document["summary"]["succeeded"] == 2
    assert len(document["files"]) == 2
    for entry in document["files"]:
        assert entry["input_sha256"]
        assert entry["stats"]["after"]["lufs"] == pytest.approx(-16.0, abs=0.1)
        assert entry["stats"]["output_clipped"] >= 0
        assert "zero_padded_frames" in entry["stats"]


def test_report_counts_match_outputs(fixture_tree, tmp_path):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in",
                           ["tone_44100_stereo", "mono_odd_tail",
                            "broken_bad_channels"])
    report_path = tmp_path / "run.json"
    run(["batch", str(inputs), "--output", str(tmp_path / "out"),
         "--report", str(report_path), "--workspace", str(tmp_path), "--quiet"])
    document = json.loads(report_path.read_text())
    outputs = list((tmp_path / "out").glob("*.wav"))
    ok_files = [entry for entry in document["files"] if entry["ok"]]
    assert len(ok_files) == len(outputs)
    assert document["summary"]["failed"] == 1


def test_inspect_prints_summary(fixture_tree, tmp_path, capsys):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in", ["tone_44100_stereo"])
    report_path = tmp_path / "run.json"
    run(["batch", str(inputs), "--output", str(tmp_path / "out"),
         "--report", str(report_path), "--workspace", str(tmp_path), "--quiet"])
    capsys.readouterr()
    assert run(["inspect", "--report", str(report_path)]) == 0
    out = capsys.readouterr().out
    for expected in ("before", "after", "gain", "LUFS", "files"):
        assert expected in out


def test_inspect_verify_passes_on_good_report(fixture_tree, tmp_path, capsys):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in",
                           ["tone_44100_stereo", "quiet_music"])
    report_path = tmp_path / "run.json"
    run(["batch", str(inputs), "--output", str(tmp_path / "out"),
         "--report", str(report_path), "--workspace", str(tmp_path), "--quiet"])
    capsys.readouterr()
    assert run(["inspect", "--report", str(report_path), "--verify"]) == 0
    assert "OK" in capsys.readouterr().out


def test_verify_detects_tampered_statistics(fixture_tree, tmp_path, capsys):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in", ["tone_44100_stereo"])
    report_path = tmp_path / "run.json"
    run(["batch", str(inputs), "--output", str(tmp_path / "out"),
         "--report", str(report_path), "--workspace", str(tmp_path), "--quiet"])
    document = json.loads(report_path.read_text())
    document["files"][0]["stats"]["after"]["lufs"] += 5.0
    report_path.write_text(json.dumps(document))
    capsys.readouterr()
    assert run(["inspect", "--report", str(report_path), "--verify"]) == 1
    assert "lufs_mismatch" in capsys.readouterr().out


def test_verify_detects_missing_output(fixture_tree, tmp_path, capsys):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in", ["tone_44100_stereo"])
    report_path = tmp_path / "run.json"
    run(["batch", str(inputs), "--output", str(tmp_path / "out"),
         "--report", str(report_path), "--workspace", str(tmp_path), "--quiet"])
    for path in (tmp_path / "out").glob("*.wav"):
        path.unlink()
    capsys.readouterr()
    assert run(["verify", "--report", str(report_path),
                "--output-dir", str(tmp_path / "out")]) == 1
    assert "output_missing" in capsys.readouterr().out


def test_inspect_missing_report_exits_three(tmp_path):
    assert run(["inspect", "--report", str(tmp_path / "nope.json")]) == 3


def test_inspect_invalid_json_exits_three(tmp_path):
    bad = tmp_path / "bad.json"
    bad.write_text("{not json")
    assert run(["inspect", "--report", str(bad)]) == 3


def test_dry_run_writes_nothing(fixture_tree, tmp_path, capsys):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in", ["tone_44100_stereo"])
    code = run(["batch", str(inputs), "--output", str(tmp_path / "out"),
                "--workspace", str(tmp_path), "--dry-run"])
    assert code == 0
    assert not (tmp_path / "out").exists()
    assert "->" in capsys.readouterr().out


def test_manifest_input_works(fixture_tree, tmp_path):
    inputs = copy_fixtures(fixture_tree, tmp_path / "in",
                           ["tone_44100_stereo", "tone_48000_51"])
    manifest = tmp_path / "inputs.txt"
    manifest.write_text("in/tone_44100_stereo.wav\nin/tone_48000_51.wav\n")
    code = run(["batch", "--manifest", str(manifest),
                "--output", str(tmp_path / "out"),
                "--workspace", str(tmp_path), "--quiet"])
    assert code == 0
    assert len(list((tmp_path / "out").glob("*.wav"))) == 2


def test_report_paths_are_relative_and_portable(fixture_tree, tmp_path, monkeypatch):
    # A report describes the delivery set, not this machine's layout: paths
    # are stored relative to their roots so the file can be compared across
    # runs and machines.
    monkeypatch.chdir(tmp_path)
    inputs = copy_fixtures(fixture_tree, tmp_path / "in", ["tone_44100_stereo"])
    report_path = tmp_path / "reports" / "run.json"
    code = run(["batch", str(inputs), "--output", str(tmp_path / "out"),
                "--report", str(report_path), "--workspace", str(tmp_path),
                "--quiet"])
    assert code == 0
    document = json.loads(report_path.read_text())
    assert document["outputs_root"] == "out"
    entry = document["files"][0]
    assert not Path(entry["output"]).is_absolute()
    assert entry["output"] == "tone_44100_stereo.wav"
