from __future__ import annotations

from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from mixer.engine import Executor


def test_damaged_wav_is_io_error_and_no_output(
    show_root: Path, show_manifest_data, write_manifest, invoke, tmp_path: Path
) -> None:
    broken = show_root / "broken.wav"
    broken.write_bytes(b"not a wave file")
    data = show_manifest_data
    data["tracks"][0]["file"] = broken.name
    path = write_manifest(data, "broken_case.json")
    output = tmp_path / "broken.wav"
    code, payload = invoke("render", path, "--out", str(output))
    assert code == 4
    assert payload["error"]["type"] == "io"
    assert not output.exists()


def test_unsupported_channels_is_render_error(
    show_root: Path, show_manifest_data, write_manifest, invoke, tmp_path: Path
) -> None:
    channels = show_root / "three_channels.wav"
    sf.write(channels, np.zeros((1024, 3), dtype=np.float32), 48000, format="WAV", subtype="FLOAT")
    data = show_manifest_data
    data["tracks"][0]["file"] = channels.name
    path = write_manifest(data, "three_channels.json")
    output = tmp_path / "three.wav"
    code, payload = invoke("render", path, "--out", str(output))
    assert code == 3
    assert payload["error"]["details"]["channels"] == 3
    assert not output.exists()


def test_unwritable_output_directory_exit_4(
    show_root: Path, invoke, tmp_path: Path
) -> None:
    blocker = tmp_path / "blocker"
    blocker.write_text("not a directory", encoding="utf-8")
    output = blocker / "mix.wav"
    code, payload = invoke("render", str(show_root / "mix.json"), "--out", str(output))
    assert code == 4
    assert payload["error"]["type"] == "io"
    assert not output.exists()


def test_report_file_and_track_errors(show_root: Path, invoke, tmp_path: Path) -> None:
    code, payload = invoke("report", str(tmp_path / "missing.json"))
    assert code == 4
    assert payload["error"]["type"] == "io"

    report = tmp_path / "report.json"
    code, _ = invoke(
        "render",
        str(show_root / "mix.json"),
        "--out",
        str(tmp_path / "mix.wav"),
        "--report",
        str(report),
    )
    assert code == 0
    code, payload = invoke("report", str(report), "--track", "does-not-exist")
    assert code == 2
    assert payload["error"]["type"] == "manifest"


def test_parity_over_tolerance_exit_3(show_root: Path, invoke, monkeypatch) -> None:
    original = Executor.run

    def shifted_run(self, chunk_frames, *, collect_audio=True, sink=None):
        if not collect_audio and sink is not None:
            def shifted(block: np.ndarray) -> None:
                sink(block + np.float32(0.001))

            return original(self, chunk_frames, collect_audio=False, sink=shifted)
        return original(self, chunk_frames, collect_audio=collect_audio, sink=sink)

    monkeypatch.setattr(Executor, "run", shifted_run)
    code, payload = invoke(
        "parity",
        str(show_root / "mix.json"),
        "--chunk",
        "4096",
        "--tolerance",
        "1e-6",
    )
    assert code == 3
    assert payload["ok"] is False
    assert payload["max_abs_diff"] > 1e-6
