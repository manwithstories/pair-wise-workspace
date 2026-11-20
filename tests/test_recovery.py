"""Recovery, resume, retry and corruption paths end to end.

These tests drive the real CLI entry point because the acceptance criteria are
stated in terms of exit codes and printed metrics.  They also assert the memory
bounds and the digest-stability guarantees that make resumption safe.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from mediacore.cli import main

BASE_ARGS = [
    "demo", "--codec", "h264", "--duration", "4", "--bitrate", "900000",
    "--seed", "41",
]


def _run(args, capsys):
    code = main(args)
    out = capsys.readouterr()
    return code, out.out, out.err


def _manifest(out: Path) -> dict:
    return json.loads((out / "manifest.json").read_text())


def test_clean_demo_prints_required_metrics(tmp_path, capsys):
    out = tmp_path / "demo"
    code, stdout, _ = _run(BASE_ARGS + ["--out", str(out)], capsys)
    assert code == 0
    assert "target_bps=900000" in stdout
    assert "actual_bps=" in stdout and "deviation_pct=" in stdout
    assert "video_psnr_db=" in stdout and "audio_rms=" in stdout
    assert "manifest_alignment_max_ms=" in stdout
    assert "random_access_max_ms=" in stdout
    manifest = _manifest(out)
    assert abs(manifest["deviation_pct"]) <= 15.0
    assert manifest["alignment"]["max_alignment_ms"] <= 40.0
    assert manifest["index"]["random_access_max_ms"] <= 40.0
    assert manifest["quality"]["video"]["finite"] is True
    assert manifest["segments"]


def test_verify_accepts_matching_manifest_and_index(tmp_path, capsys):
    out = tmp_path / "demo"
    assert _run(BASE_ARGS + ["--out", str(out)], capsys)[0] == 0
    code, stdout, _ = _run(
        ["verify", "--manifest", str(out / "manifest.json"),
         "--index", str(out / "index.json")], capsys)
    assert code == 0
    assert "digest_matches=True" in stdout


def test_interrupt_then_resume_reaches_the_same_manifest_digest(tmp_path, capsys):
    out = tmp_path / "resume"
    code, _, stderr = _run(
        BASE_ARGS + ["--out", str(out), "--interrupt-after", "2"], capsys)
    assert code == 3
    assert "interrupted" in stderr
    assert not (out / "manifest.json").exists()
    checkpoint = json.loads((out / "run" / "checkpoint.json").read_text())
    committed = [entry["stage"] for entry in checkpoint["commits"]]
    assert "derive" in committed

    code, _, _ = _run(BASE_ARGS + ["--out", str(out)], capsys)
    assert code == 0
    resumed_manifest = _manifest(out)
    assert resumed_manifest["checkpoint"]["resumed"] is True

    fresh = tmp_path / "fresh"
    assert _run(BASE_ARGS + ["--out", str(fresh)], capsys)[0] == 0
    assert _manifest(fresh)["manifest_digest"] == resumed_manifest["manifest_digest"]


def test_repeated_identical_runs_are_idempotent(tmp_path, capsys):
    out = tmp_path / "repeat"
    digests = []
    for _ in range(3):
        code, _, _ = _run(BASE_ARGS + ["--out", str(out), "--quiet"], capsys)
        assert code == 0
        digests.append(_manifest(out)["manifest_digest"])
    assert len(set(digests)) == 1


def test_changed_configuration_refuses_checkpoint_with_reason(tmp_path, capsys):
    out = tmp_path / "conflict"
    assert _run(BASE_ARGS + ["--out", str(out), "--quiet"], capsys)[0] == 0
    changed = list(BASE_ARGS)
    changed[-1] = "42"  # --seed
    code, _, stderr = _run(changed + ["--out", str(out)], capsys)
    assert code == 2
    assert "checkpoint_conflict" in stderr
    assert "inputs_changed" in stderr or "config_changed" in stderr


def test_corrupt_tail_is_located_and_exits_2(tmp_path, capsys):
    out = tmp_path / "corrupt"
    code, stdout, stderr = _run(
        BASE_ARGS + ["--out", str(out), "--corrupt-tail"], capsys)
    assert code == 2
    assert "corrupt_input" in stderr and "offset" in stderr
    manifest = _manifest(out)
    offset = manifest["video"]["truncated_tail_offset"]
    assert offset is not None
    assert 0 < offset < manifest["video"]["bytes"]
    assert any(i["code"].startswith("truncated") for i in manifest["issues"])
    assert all(i["offset"] is not None for i in manifest["issues"])
    # verification refuses to bless a truncated recording
    code, _, stderr = _run(
        ["verify", "--manifest", str(out / "manifest.json"),
         "--index", str(out / "index.json")], capsys)
    assert code == 2
    assert str(offset) in stderr


def test_scan_reports_truncation_offset(tmp_path, capsys):
    out = tmp_path / "scan"
    assert _run(BASE_ARGS + ["--out", str(out), "--corrupt-tail", "--quiet"],
                capsys)[0] == 2
    video = out / "input" / "video.h264"
    code, stdout, _ = _run(
        ["scan", "--input", str(video), "--codec", "h264", "--json"], capsys)
    assert code == 0
    payload = json.loads(stdout)
    assert payload["truncated_tail_offset"] is not None
    assert payload["issues"]


def test_missing_manifest_is_an_input_error(tmp_path, capsys):
    code, _, stderr = _run(
        ["verify", "--manifest", str(tmp_path / "nope.json")], capsys)
    assert code == 2
    assert "input_missing" in stderr


def test_pipeline_reports_bounded_resources(tmp_path, capsys):
    out = tmp_path / "bounds"
    assert _run(BASE_ARGS + ["--out", str(out), "--quiet"], capsys)[0] == 0
    manifest = _manifest(out)
    resource = manifest["resource"]
    assert resource["memory_limit_bytes"] <= 128 * 1024 * 1024
    assert resource["scan_peak_read_buffer_bytes"] <= resource["memory_limit_bytes"]
    assert resource["read_queue_high_water"] <= resource["read_queue_max_items"]


def test_invalid_arguments_exit_2(tmp_path):
    with pytest.raises(SystemExit) as excinfo:
        main(["demo", "--codec", "vp9", "--out", str(tmp_path)])
    assert excinfo.value.code == 2
    assert not (tmp_path / "manifest.json").exists()


def test_corrupted_scan_block_is_rescanned_not_reused(tmp_path, capsys):
    out = tmp_path / "tamper"
    assert _run(BASE_ARGS + ["--out", str(out), "--quiet"], capsys)[0] == 0
    baseline = _manifest(out)["manifest_digest"]

    scan_path = out / "run" / "scan.json"
    raw = json.loads(scan_path.read_text())
    raw["units"][5]["offset"] = raw["units"][4]["offset"]
    scan_path.write_text(json.dumps(raw))

    code, _, _ = _run(BASE_ARGS + ["--out", str(out), "--quiet"], capsys)
    assert code == 0
    manifest = _manifest(out)
    # the tampered block was rejected (digest mismatch) and the run rescanned
    assert "scan" not in manifest["checkpoint"]["reused"]
    assert manifest["resource"]["reuse_reasons"]["scan"]
    assert manifest["manifest_digest"] == baseline


def test_corrupted_index_block_is_rebuilt(tmp_path, capsys):
    out = tmp_path / "tamper_index"
    assert _run(BASE_ARGS + ["--out", str(out), "--quiet"], capsys)[0] == 0
    baseline = _manifest(out)["manifest_digest"]

    index_path = out / "run" / "index.json"
    raw = json.loads(index_path.read_text())
    raw["entries"][0]["pts"] = 9999
    index_path.write_text(json.dumps(raw))

    code, _, _ = _run(BASE_ARGS + ["--out", str(out), "--quiet"], capsys)
    assert code == 0
    manifest = _manifest(out)
    assert manifest["resource"]["reuse_reasons"]["derive"]
    assert manifest["manifest_digest"] == baseline


def test_partial_scan_block_falls_back_to_full_scan(tmp_path, capsys):
    out = tmp_path / "partial"
    assert _run(BASE_ARGS + ["--out", str(out), "--quiet"], capsys)[0] == 0
    scan_path = out / "run" / "scan.json"
    scan_path.write_text(scan_path.read_text()[: len(scan_path.read_text()) // 2])
    code, _, _ = _run(BASE_ARGS + ["--out", str(out), "--quiet"], capsys)
    assert code == 0
    assert "scan" not in _manifest(out)["checkpoint"]["reused"]
