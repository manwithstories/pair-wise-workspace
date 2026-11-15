import json
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from audio_pipeline.cli import main
from audio_pipeline.job import BatchConfig, run_batch, write_report
from audio_pipeline.verify import verify_report


REPORT_FIELDS = {
    "in_rate",
    "out_rate",
    "out_channels",
    "rms_dbfs_before",
    "rms_dbfs_after",
    "lufs_before",
    "lufs_after",
    "true_peak_dbtp",
    "clipped_before",
    "clipped_after",
    "kernel_fingerprint",
}


def _tone(rate: int, channels: int, amplitude: float = 0.12, duration: float = 0.8):
    time = np.arange(int(rate * duration)) / rate
    signal = amplitude * np.sin(2 * np.pi * 300 * time) + 0.03 * np.sin(
        2 * np.pi * 1100 * time
    )
    return np.stack([signal] * channels, axis=1)


def _make_inputs(directory: Path) -> None:
    directory.mkdir(parents=True)
    sf.write(directory / "a_8k_u8.wav", _tone(8000, 1)[:, :1], 8000, subtype="PCM_U8")
    sf.write(directory / "b_44k_stereo.wav", _tone(44100, 2), 44100, subtype="PCM_24")
    clipped = _tone(48000, 2)
    clipped[1000:1120, 0] = 0.999
    sf.write(directory / "c_48k_clipped.wav", clipped, 48000, subtype="PCM_24")


def test_batch_report_verify_and_determinism(tmp_path):
    input_dir = tmp_path / "in"
    output_dir = tmp_path / "out"
    _make_inputs(input_dir)
    config = BatchConfig(input_dir, output_dir, target_lufs=-20.0, block_size=1234)
    first = run_batch(config)
    second = run_batch(config)
    assert first == second
    assert first["summary"] == {"total": 3, "ok": 3, "failed": 0}
    assert sum(record["clipped_before"] for record in first["records"]) == 120
    for record in first["records"]:
        assert REPORT_FIELDS <= record.keys()
        assert record["out_rate"] == 48000
        assert record["out_channels"] == 2
        assert record["true_peak_dbtp"] <= -1.0
        assert record["clipped_after"] == 0
        if record["in_rate"] != 48000:
            assert record["lufs_after"] == pytest.approx(-20.0, abs=0.1)
        assert (output_dir / record["output"]).is_file()
    report_path = output_dir / "report.json"
    write_report(first, report_path)
    result = verify_report(report_path, output_dir, block_size=999)
    assert result.ok and result.checked == 3

    tampered = json.loads(report_path.read_text(encoding="utf-8"))
    tampered["records"][0]["lufs_after"] += 1.0
    bad_path = output_dir / "bad_report.json"
    write_report(tampered, bad_path)
    assert main(["verify", "--report", str(bad_path), "--dir", str(output_dir)]) == 3


def test_invalid_files_are_isolated_and_have_codes(tmp_path):
    input_dir = tmp_path / "in"
    output_dir = tmp_path / "out"
    input_dir.mkdir()
    sf.write(input_dir / "valid.wav", _tone(8000, 1, amplitude=0.05)[:, :1], 8000)
    sf.write(input_dir / "empty.wav", np.empty((0, 1)), 8000)
    (input_dir / "corrupt.wav").write_bytes(b"not an audio file")
    report = run_batch(BatchConfig(input_dir, output_dir, block_size=512))
    assert report["summary"] == {"total": 3, "ok": 1, "failed": 2}
    by_name = {record["input"]: record for record in report["records"]}
    assert by_name["valid.wav"]["status"] == "ok"
    assert by_name["empty.wav"]["error_code"] == 10
    assert by_name["corrupt.wav"]["error_code"] == 11


def test_silent_file_remains_exactly_zero(tmp_path):
    input_dir = tmp_path / "in"
    output_dir = tmp_path / "out"
    input_dir.mkdir()
    sf.write(input_dir / "silence.wav", np.zeros((800, 2)), 8000, subtype="PCM_16")
    report = run_batch(BatchConfig(input_dir, output_dir, block_size=137))
    assert report["summary"]["ok"] == 1
    output, _ = sf.read(output_dir / "silence.wav", dtype="float64", always_2d=True)
    assert np.all(output == 0.0)
    assert report["records"][0]["clipped_after"] == 0


def test_readonly_output_and_cli_exit_codes(tmp_path):
    input_dir = tmp_path / "in"
    input_dir.mkdir()
    sf.write(input_dir / "a.wav", _tone(8000, 1, amplitude=0.05)[:, :1], 8000)
    readonly = tmp_path / "readonly"
    readonly.mkdir()
    readonly.chmod(0o500)
    try:
        report = run_batch(BatchConfig(input_dir, readonly))
    finally:
        readonly.chmod(0o700)
    assert report["summary"]["failed"] == 1
    assert report["records"][0]["error_code"] == 13

    assert (
        main(
            [
                "batch",
                "--in",
                str(input_dir),
                "--out",
                str(tmp_path / "x"),
                "--block",
                "0",
            ]
        )
        == 2
    )
    assert (
        main(
            [
                "batch",
                "--in",
                str(input_dir),
                "--out",
                str(tmp_path / "kernel"),
                "--target-rate",
                "3000000",
            ]
        )
        == 4
    )
