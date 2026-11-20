"""Cross-module recovery paths through the batch CLI (demo/verify).

These exercise the whole flow: synthetic capture -> RTP/RTCP parse ->
clock -> repair/backpressure -> decode with freeze fill -> aligned ladder
-> atomic manifest, plus the documented exit-status contract.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from archiveflow.cli import main

SHORT = "2.5"
TARGETS = "500000,1200000"


def run_demo(out: Path, *extra: str) -> int:
    return main(["demo", "--duration", SHORT, "--targets", TARGETS,
                 "--out", str(out), *extra])


def load(out: Path) -> dict:
    return json.loads((out / "manifest.json").read_text())


class TestRecoveryEndToEnd:
    def test_clean_capture_meets_quality_contract(self, tmp_path: Path):
        out = tmp_path / "run"
        assert run_demo(out) == 0
        m = load(out)
        # Every representation records achievable bitrate/quality targets.
        for rep in m["representations"]:
            q = m["quality"][rep["name"]]
            assert q["psnr_db"] > 30.0
            assert q["ssim"] > 0.90
        for seg in m["segments"]:
            assert abs(seg["bitrate_error_pct"]) <= 15.0
        assert m["alignment_error_ms"] <= 40.0
        assert m["pipeline"]["peak_queue_items"] <= 256
        # The verify sub-command accepts the written manifest.
        assert main(["verify", "--manifest", str(out / "manifest.json")]) == 0

    @pytest.mark.parametrize("preset", [
        "duplicates", "reorder-overflow", "loss-burst", "rtcp-conflict",
        "truncated",
    ])
    def test_repairable_corruption_presets_exit_zero(self, tmp_path: Path,
                                                      preset: str):
        out = tmp_path / preset
        assert run_demo(out, "--corrupt", preset) == 0
        m = load(out)
        rec = m["recovery"]
        if preset == "duplicates":
            assert rec["duplicates"] > 0
        if preset == "reorder-overflow":
            assert rec["late_packets"] > 0
        if preset == "loss-burst":
            assert rec["loss_bursts"] > 0
        if preset == "rtcp-conflict":
            assert m["clock"]["uncertainty_ms"] > 0
        if preset == "truncated":
            assert m["truncated"] is True
            assert any("truncated_tail" in s["flags"]
                       for s in m["segments"])

    def test_empty_capture_is_unrecoverable_exit_one(self, tmp_path: Path):
        out = tmp_path / "empty"
        assert run_demo(out, "--corrupt", "empty") == 1
        # A failed run must not register a half-finished manifest.
        assert not (out / "manifest.json").exists()

    def test_deterministic_across_runs(self, tmp_path: Path):
        a, b = tmp_path / "a", tmp_path / "b"
        assert run_demo(a, "--loss-burst", "0.4") == 0
        assert run_demo(b, "--loss-burst", "0.4") == 0
        assert load(a) == load(b)


class TestExitStatusContract:
    def test_bad_targets_exit_two(self, tmp_path: Path):
        code = main(["demo", "--duration", SHORT, "--targets", "0,-5",
                     "--out", str(tmp_path / "x")])
        assert code == 2

    def test_non_finite_duration_exit_two(self, tmp_path: Path):
        code = main(["demo", "--duration", "nan", "--targets", TARGETS,
                     "--out", str(tmp_path / "x")])
        assert code == 2

    def test_duration_out_of_range_exit_two(self, tmp_path: Path):
        code = main(["demo", "--duration", "999999", "--targets", TARGETS,
                     "--out", str(tmp_path / "x")])
        assert code == 2

    def test_queue_size_out_of_range_exit_two(self, tmp_path: Path):
        code = main(["demo", "--duration", SHORT, "--targets", TARGETS,
                     "--queue-size", "999999",
                     "--out", str(tmp_path / "x")])
        assert code == 2

    def test_segment_cap_rejected_without_any_output(self, tmp_path: Path):
        out = tmp_path / "capped"
        code = main(["demo", "--duration", SHORT, "--targets", TARGETS,
                     "--max-segments", "1", "--out", str(out)])
        assert code == 2
        assert not out.exists()  # rejected before any artifact is created

    def test_max_segments_out_of_range_exit_two(self, tmp_path: Path):
        code = main(["demo", "--duration", SHORT, "--targets", TARGETS,
                     "--max-segments", "999999",
                     "--out", str(tmp_path / "x")])
        assert code == 2

    def test_verify_missing_manifest_exit_one(self, tmp_path: Path):
        code = main(["verify", "--manifest",
                     str(tmp_path / "nope.json")])
        assert code == 1

    def test_verify_rejects_manifest_with_missing_segment(
            self, tmp_path: Path):
        out = tmp_path / "run"
        assert run_demo(out) == 0
        m = load(out)
        # Delete one referenced segment file; validation must fail (exit 1)
        # rather than silently accept a broken hand-off.
        (out / m["segments"][0]["file"]).unlink()
        assert main(["verify",
                     "--manifest", str(out / "manifest.json")]) == 1


    def test_missing_encoder_reports_capability_exit_one(
            self, tmp_path: Path, monkeypatch):
        import av
        real_codec = av.codec.Codec

        def fake_codec(name, mode=None):
            if mode == "w":
                raise av.codec.codec.UnknownCodecError(
                    "simulated missing encoder", -1)
            return real_codec(name, mode)

        monkeypatch.setattr(av.codec, "Codec", fake_codec)
        out = tmp_path / "noencoder"
        # Missing codec is a capability failure (exit 1), not a crash.
        assert run_demo(out) == 1
        assert not (out / "manifest.json").exists()


class TestAtomicManifest:
    def test_failed_run_preserves_previous_complete_manifest(
            self, tmp_path: Path):
        out = tmp_path / "run"
        assert run_demo(out) == 0
        good = (out / "manifest.json").read_text()
        # A second, unrecoverable run must not overwrite the good manifest.
        assert run_demo(out, "--corrupt", "empty") == 1
        assert (out / "manifest.json").read_text() == good
        assert not list(out.glob(".manifest-*.tmp"))
