from __future__ import annotations

import hashlib
import math
from pathlib import Path

import numpy as np


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def test_render_stream_parity_determinism(show_root: Path, invoke, tmp_path: Path) -> None:
    manifest = str(show_root / "mix.json")
    first = tmp_path / "first.wav"
    second = tmp_path / "second.wav"
    streamed = tmp_path / "stream.wav"
    report = tmp_path / "report.json"
    stream_report = tmp_path / "report_stream.json"
    code, payload = invoke("render", manifest, "--out", str(first), "--report", str(report))
    assert code == 0
    assert payload["chunks"] == 1
    assert payload["mix"]["clip_count_before_limiter"] > 0
    assert payload["mix"]["clip_count_after_limiter"] == 0
    assert payload["mix"]["peak_dbfs"] < 0.0
    code, _ = invoke("render", manifest, "--out", str(second))
    assert code == 0
    code, stream_payload = invoke(
        "stream",
        manifest,
        "--chunk",
        "1024",
        "--out",
        str(streamed),
        "--report",
        str(stream_report),
    )
    assert code == 0
    assert _digest(first) == _digest(second) == _digest(streamed)
    assert stream_payload["tracks"] == payload["tracks"]
    assert stream_payload["mix"] == payload["mix"]

    code, parity = invoke("parity", manifest, "--chunk", "1024", "--tolerance", "1e-6")
    assert code == 0
    assert parity["ok"] is True
    assert parity["max_abs_diff"] == 0.0
    assert parity["chunks"] == math.ceil(4.0 * 48000 / 1024)

    code, track = invoke("report", str(report), "--track", "host")
    assert code == 0
    assert track["track"]["id"] == "host"
    assert track["track"]["peak_dbfs"] < 0.0
    assert track["track"]["rms_dbfs"] < 0.0
    assert track["track"]["gain_db_min"] <= track["track"]["gain_db_max"]
    assert track["track"]["automation_breakpoints"] == 4


def test_stats_silence_empty_and_resample_lengths(show_root: Path, invoke, tmp_path: Path) -> None:
    report_path = tmp_path / "report.json"
    code, payload = invoke(
        "render",
        str(show_root / "mix.json"),
        "--out",
        str(tmp_path / "mix.wav"),
        "--report",
        str(report_path),
    )
    assert code == 0
    tracks = {track["id"]: track for track in payload["tracks"]}
    for track_id in ("room", "spot"):
        assert tracks[track_id]["peak_dbfs"] == "-inf"
        assert tracks[track_id]["rms_dbfs"] == "-inf"
    assert tracks["spot"]["samples_in"] == 0
    assert tracks["spot"]["samples_out"] == 4 * 48000
    assert tracks["host"]["resample_ratio"] > 1.0
    assert tracks["sfx"]["resample_ratio"] == 0.5
    assert tracks["host"]["automation_points"] == 4000
    assert payload["automation_points"] == 4000
    assert payload["mix"]["duration_s"] == 4.0


def test_stream_failure_removes_partial_output(
    show_root: Path, show_manifest_data, write_manifest, invoke, tmp_path: Path
) -> None:
    nan_path = show_root / "nan.wav"
    data = np.zeros((4096, 1), dtype=np.float32)
    data[100] = np.nan
    import soundfile as sf

    sf.write(nan_path, data, 48000, format="WAV", subtype="FLOAT")
    manifest = show_manifest_data
    manifest["tracks"][0]["file"] = nan_path.name
    path = write_manifest(manifest, "nan_case.json")
    output = tmp_path / "nan_stream.wav"
    code, payload = invoke("stream", path, "--chunk", "512", "--out", str(output))
    assert code == 3
    assert payload["error"]["type"] == "render"
    assert not output.exists()
    assert not list(tmp_path.glob(".mixer-*"))
