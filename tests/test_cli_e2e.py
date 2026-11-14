from __future__ import annotations

import hashlib
import json
import re
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

from tlcomp.compositor import Compositor
from tlcomp.timeline import load_timeline

ROOT = Path(__file__).resolve().parents[1]


def run_cli(*arguments: str) -> tuple[int, dict]:
    completed = subprocess.run(
        [sys.executable, "-m", "tlcomp", *arguments],
        cwd=ROOT,
        text=True,
        capture_output=True,
        check=False,
    )
    output = completed.stdout.strip()
    assert output, completed.stderr
    return completed.returncode, json.loads(output)


def test_render_summary_hash_and_parallel_identity(project_factory, tmp_path: Path) -> None:
    project = project_factory(frames=8)
    single = tmp_path / "single"
    parallel = tmp_path / "parallel"
    code, result = run_cli("render", str(project), "--out", str(single), "--workers", "1")
    assert code == 0 and result["frames"] == 8
    code, result = run_cli("render", str(project), "--out", str(parallel), "--workers", "2")
    assert code == 0 and result["frames"] == 8
    first_bytes = (single / "manifest.jsonl").read_bytes()
    second_bytes = (parallel / "manifest.jsonl").read_bytes()
    assert first_bytes == second_bytes
    rows = [json.loads(line) for line in first_bytes.splitlines()]
    assert len(rows) == 8
    assert [row["frame"] for row in rows] == list(range(8))
    assert [row["pts_ms"] for row in rows] == sorted(row["pts_ms"] for row in rows)
    assert all(re.fullmatch(r"[0-9a-f]{64}", row["sha256"]) for row in rows)
    assert all(0.0 <= row["coverage"] <= 1.0 for row in rows)
    assert list(single.iterdir()) == [single / "manifest.jsonl"]
    composed = Compositor(load_timeline(project)).render(0)
    assert rows[0]["sha256"] == hashlib.sha256(composed.tobytes(order="C")).hexdigest()


def test_verify_diff_and_tamper_exit_codes(project_factory, tmp_path: Path) -> None:
    project = project_factory(frames=6)
    output = tmp_path / "out"
    assert run_cli("render", str(project), "--out", str(output))[0] == 0
    manifest = output / "manifest.jsonl"
    baseline = tmp_path / "baseline.jsonl"
    shutil.copyfile(manifest, baseline)
    code, result = run_cli("verify", str(project), "--manifest", str(manifest))
    assert code == 0
    assert result == {"checked": 6, "first_bad_frame": None, "mismatch": 0}

    rows = [json.loads(line) for line in manifest.read_text().splitlines()]
    rows[3]["mean_rgb"] = [1.0, 2.0, 3.0]
    manifest.write_text("".join(json.dumps(row, sort_keys=True, separators=(",", ":")) + "\n" for row in rows))
    code, result = run_cli("verify", str(project), "--manifest", str(manifest))
    assert code == 1
    assert result["mismatch"] == 1 and result["first_bad_frame"] == 3
    code, result = run_cli("diff", str(baseline), str(manifest))
    assert code == 1
    assert result == {"diff_frames": 1, "intervals": [{"start": 3, "end": 3}]}
    code, result = run_cli("diff", str(baseline), str(baseline))
    assert code == 0 and result == {"diff_frames": 0, "intervals": []}


def test_resume_truncated_line_and_corrupt_state(project_factory, tmp_path: Path) -> None:
    project = project_factory(frames=8)
    baseline_dir = tmp_path / "baseline"
    run_cli("render", str(project), "--out", str(baseline_dir), "--workers", "2")
    baseline = (baseline_dir / "manifest.jsonl").read_bytes()
    lines = baseline.splitlines(keepends=True)

    resume_dir = tmp_path / "resume"
    resume_dir.mkdir()
    resume_manifest = resume_dir / "manifest.jsonl"
    resume_manifest.write_bytes(b"".join(lines[:4]) + lines[4][:17])
    code, result = run_cli("render", str(project), "--out", str(resume_dir), "--workers", "2", "--resume")
    assert code == 0 and result["frames"] == 8
    assert resume_manifest.read_bytes() == baseline

    corrupt_dir = tmp_path / "corrupt"
    corrupt_dir.mkdir()
    corrupt_manifest = corrupt_dir / "manifest.jsonl"
    corrupt_data = lines[0] + lines[0]
    corrupt_manifest.write_bytes(corrupt_data)
    code, result = run_cli("render", str(project), "--out", str(corrupt_dir), "--resume")
    assert code == 4 and result["kind"] == "resume"
    assert corrupt_manifest.read_bytes() == corrupt_data


def test_failure_classes_are_atomic(project_factory, tmp_path: Path) -> None:
    project = project_factory(frames=4)
    data = json.loads(project.read_text())
    data["fps"] = "bad"
    project.write_text(json.dumps(data))
    invalid_out = tmp_path / "invalid"
    code, result = run_cli("render", str(project), "--out", str(invalid_out))
    assert code == 2 and result["kind"] == "project"
    assert not (invalid_out / "manifest.jsonl").exists()

    project = project_factory(frames=4, name="missing.json")
    data = json.loads(project.read_text())
    data["layers"][0]["path"] = "assets/not-there.png"
    project.write_text(json.dumps(data))
    missing_out = tmp_path / "missing"
    code, result = run_cli("render", str(project), "--out", str(missing_out))
    assert code == 3 and result["kind"] == "asset"
    assert not (missing_out / "manifest.jsonl").exists()

    project = project_factory(frames=4, name="bad-image.json")
    (project.parent / "assets" / "base.png").write_bytes(b"not a png")
    bad_out = tmp_path / "bad-image"
    code, result = run_cli("render", str(project), "--out", str(bad_out))
    assert code == 3 and result["kind"] == "asset"
    assert not (bad_out / "manifest.jsonl").exists()

    project = project_factory(frames=4, name="cue.json", with_layers=False, layers=[])
    data = json.loads(project.read_text())
    data["audio"] = {"path": "missing.wav", "cues": [4]}
    project.write_text(json.dumps(data))
    code, result = run_cli("audio-check", str(project), "--out", str(tmp_path / "audio.json"))
    assert code == 2 and result["kind"] == "project"
    assert run_cli("render", str(project), "--out", str(tmp_path / "workers"), "--workers", "0")[0] == 2


def test_full_fixture_acceptance_chain(tmp_path: Path) -> None:
    data_dir = tmp_path / "demo"
    out_dir = tmp_path / "demo-out"
    audio_file = out_dir / "audio.json"
    code, result = run_cli("fixtures", "--out", str(data_dir), "--frames", "12", "--fps", "30000/1001")
    assert code == 0 and result["cues"] == 6
    project = data_dir / "project.json"
    code, result = run_cli("render", str(project), "--out", str(out_dir), "--workers", "2")
    assert code == 0 and result["frames"] == 12
    code, result = run_cli("verify", str(project), "--manifest", str(out_dir / "manifest.jsonl"))
    assert code == 0 and result == {"checked": 12, "first_bad_frame": None, "mismatch": 0}
    code, result = run_cli("audio-check", str(project), "--out", str(audio_file))
    assert code == 0 and result["cues"] == 6 and result["worst_cue"] == 2
    assert result["max_abs_error_ms"] == 0.0
    assert audio_file.exists()
    code, result = run_cli("diff", str(out_dir / "manifest.jsonl"), str(out_dir / "manifest.jsonl"))
    assert code == 0 and result == {"diff_frames": 0, "intervals": []}
