"""处理链路测试：正常/损坏样例、退出码、传输恢复与音频变换单元。"""
from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import numpy as np
import pytest

from live_delivery import audio, decode
from live_delivery.cli import main as cli_main
from live_delivery.errors import DecodeError, ManifestError
from live_delivery.manifest import Manifest, SegmentRef, load_manifest
from live_delivery.transport import ingest

ROOT = Path(__file__).resolve().parent.parent


@pytest.fixture(scope="session")
def samples(tmp_path_factory) -> Path:
    out = tmp_path_factory.mktemp("samples")
    subprocess.run(
        [sys.executable, str(ROOT / "scripts" / "make_samples.py"),
         "--out", str(out), "--seed", "17"],
        check=True,
    )
    return out


def run_process(samples: Path, manifest: str, tmp_path: Path, extra=()):
    out = tmp_path / "out.wav"
    rep = tmp_path / "report.json"
    argv = ["process", "--manifest", str(samples / manifest), "--out", str(out),
            "--rate", "48000", "--layout", "stereo", "--target-lufs", "-16",
            "--max-queue-mib", "8", "--json", str(rep), *extra]
    code = cli_main(argv)
    report = json.loads(rep.read_text()) if rep.is_file() else None
    return code, out, report


def test_normal_hls(samples, tmp_path):
    code, out, rep = run_process(samples, "hls.m3u8", tmp_path)
    assert code == 0
    assert abs(rep["bitrate_delta_pct"]) <= 12.0
    assert abs(rep["integrated_lufs"] - rep["target_lufs"]) <= 1.5
    assert rep["manifest_alignment_ms"] <= 20.0
    assert rep["duplicate_segments"] == 0
    assert rep["recovered_gaps"] == 0
    assert rep["peak_buffer_bytes"] <= 2 * 8 * 1024 * 1024
    assert rep["true_peak_dbfs"] <= -1.0
    assert cli_main(["verify", "--report", str(tmp_path / "report.json")]) == 0


def test_normal_dash(samples, tmp_path):
    code, _out, rep = run_process(samples, "dash.mpd", tmp_path)
    assert code == 0
    assert rep["kind"] == "dash"
    assert abs(rep["integrated_lufs"] - rep["target_lufs"]) <= 1.5


def test_duplicate_dedup(samples, tmp_path):
    code, _out, rep = run_process(samples, "hls_dup.m3u8", tmp_path)
    assert code == 0
    assert rep["duplicate_segments"] == 1


def test_conflict_isolated(samples, tmp_path):
    code, _out, rep = run_process(samples, "hls_conflict.m3u8", tmp_path)
    assert code == 0
    assert rep["isolated_segments"] == 1
    assert rep["recovered_gaps"] >= 1
    assert rep["manifest_alignment_ms"] > 0
    assert any(g["reason"] == "conflict" for g in rep["gaps"])


def test_gap_recovered(samples, tmp_path):
    code, _out, rep = run_process(samples, "hls_gap.m3u8", tmp_path)
    assert code == 0
    assert rep["missing_segments"] == 1
    assert rep["recovered_gaps"] >= 1
    assert rep["manifest_alignment_ms"] > 0
    assert rep["silence_ratio"] > 0.1


def test_gap_fail_policy_exit3(samples, tmp_path):
    code, _out, rep = run_process(samples, "hls_gap.m3u8", tmp_path,
                                  extra=["--gap-policy", "fail"])
    assert code == 3
    assert rep["unrecoverable_errors"] >= 1


def test_invalid_manifest_exit2(samples, tmp_path):
    bad = tmp_path / "bad.m3u8"
    bad.write_text("#EXTM3U\n#EXTINF:-1.0,\nseg_000.wav\n", encoding="utf-8")
    code = cli_main(["process", "--manifest", str(bad),
                     "--out", str(tmp_path / "x.wav")])
    assert code == 2


def test_external_uri_rejected(tmp_path):
    bad = tmp_path / "ext.m3u8"
    bad.write_text("#EXTM3U\n#EXTINF:2.0,\nhttp://evil.example/seg.ts\n",
                   encoding="utf-8")
    with pytest.raises(ManifestError):
        load_manifest(bad)


def test_verify_threshold_exit4(tmp_path):
    rep = tmp_path / "bad_report.json"
    rep.write_text(json.dumps({
        "target_lufs": -16.0, "integrated_lufs": -30.0,
        "bitrate_delta_pct": 50.0, "manifest_alignment_ms": 0.0,
        "unrecoverable_errors": 0,
    }), encoding="utf-8")
    assert cli_main(["verify", "--report", str(rep)]) == 4
    assert cli_main(["verify", "--report", str(tmp_path / "missing.json")]) == 2


def test_transport_reorder_and_late(tmp_path):
    for name in ("a", "b", "c"):
        (tmp_path / name).write_bytes(name.encode() * 8)
    refs = [
        SegmentRef(5, "a", 2.0, 10.0),   # 先到，提交至 12s
        SegmentRef(4, "b", 2.0, 8.0),    # 乱序但在窗口内 → 重排
        SegmentRef(0, "c", 2.0, 0.0),    # 迟到超窗 → 丢弃
    ]
    result = ingest(Manifest("hls", tmp_path / "m.m3u8", tmp_path, 2.0, refs))
    assert [d.ref.sequence for d in result.deliveries] == [4, 5]
    assert result.late_dropped == 1


def test_transport_conflict_poisons_interval(tmp_path):
    (tmp_path / "a").write_bytes(b"first-copy")
    (tmp_path / "b").write_bytes(b"second-copy")
    refs = [SegmentRef(0, "a", 2.0, 0.0), SegmentRef(0, "b", 2.0, 2.0)]
    result = ingest(Manifest("hls", tmp_path / "m.m3u8", tmp_path, 2.0, refs))
    assert result.conflicts == 1
    assert result.deliveries == []
    assert result.gaps[0].reason == "conflict"
    assert result.gaps[0].duration == 2.0


def test_decode_failure_raises(tmp_path):
    bad = tmp_path / "junk.wav"
    bad.write_bytes(b"not a media file at all")
    from live_delivery.transport import Delivery
    with pytest.raises(DecodeError):
        list(decode.decode(Delivery(SegmentRef(0, "junk.wav", 2.0, 0.0),
                                    bad, "x", bad.stat().st_size)))


def test_resample_and_layout():
    data = np.random.default_rng(0).standard_normal((2, 4410)).astype(np.float32)
    out = audio.resample(data, 44100, 48000)
    assert out.shape == (2, 4800)
    tail = audio.resample(data[:, :37], 44100, 48000)  # 末尾不足一帧
    assert tail.shape[1] >= 1
    mono = np.ones((1, 100), np.float32)
    stereo = audio.normalize_layout(mono, "mono", "stereo")
    assert stereo.shape == (2, 100)
    assert audio.normalize_layout(stereo, "stereo", "mono").shape == (1, 100)


def test_loudness_and_gain_limit():
    rate = 48000
    t = np.arange(rate * 2) / rate
    x = (0.1 * np.sin(2 * np.pi * 1000 * t)).astype(np.float32)
    meter = audio.LoudnessMeter(rate, 2)
    meter.update(np.stack([x, x]))
    lufs = meter.integrated()
    assert lufs is not None and abs(lufs - (-23.7)) < 1.0
    # 峰值上限约束增益：-2 dBFS 峰值最多提升 1 dB
    assert audio.gain_db(-30.0, -16.0, -2.0, -1.0) == pytest.approx(1.0)
