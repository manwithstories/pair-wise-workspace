"""End-to-end and failure-mode tests for the stageflow pipeline."""

from __future__ import annotations

import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np
import soundfile as sf

from stageflow.cli import main
from stageflow.clock import MASTER_RATE

RATE = 48000
TICKS = MASTER_RATE  # 1 second delivery


def write_tone(path: Path, rate: int, seconds: float, freq: float, channels: int) -> None:
    t = np.arange(int(rate * seconds), dtype=np.float64) / rate
    x = 0.2 * np.sin(2 * np.pi * freq * t)
    sf.write(path, np.repeat(x[:, None], channels, 1).astype(np.float32), rate,
             subtype="FLOAT")


def make_manifest(directory: Path, tracks: list[dict], **delivery_overrides) -> Path:
    delivery = {
        "sample_rate": RATE, "channels": 2, "fps": 30, "width": 160, "height": 90,
        "start_tick": 0, "duration_ticks": TICKS,
        "tracks": [t["id"] for t in tracks],
    }
    delivery.update(delivery_overrides)
    doc = {"job": "t", "delivery": delivery, "tracks": tracks}
    path = directory / "show.json"
    path.write_text(json.dumps(doc), encoding="utf-8")
    return path


def one_track(seg_ticks: int = TICKS, path: str = "a.wav", **kw) -> dict:
    track = {
        "id": "a", "path": path, "offset_ticks": 0,
        "segments": [{"start_tick": 0, "duration_ticks": seg_ticks}],
    }
    track.update(kw)
    return track


class StageflowTestBase(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self._tmp.name)
        self.out = self.dir / "out"

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def render(self, manifest: Path, *extra: str) -> tuple[int, str, str]:
        stdout, stderr = io.StringIO(), io.StringIO()
        argv = ["render", "--manifest", str(manifest), "--out", str(self.out),
                "--block-frames", "2048", *extra]
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = main(argv)
        return code, stdout.getvalue(), stderr.getvalue()

    @staticmethod
    def fields(text: str) -> dict[str, str]:
        out = {}
        for line in text.splitlines():
            if "=" in line:
                k, _, v = line.partition("=")
                out[k] = v
        return out


class SuccessTests(StageflowTestBase):
    def test_success_end_to_end(self) -> None:
        write_tone(self.dir / "a.wav", RATE, 1.0, 440.0, 2)
        write_tone(self.dir / "b.wav", 44100, 1.0, 550.0, 1)
        m = make_manifest(self.dir, [
            one_track(),
            {"id": "b", "path": "b.wav", "offset_ticks": 0,
             "segments": [{"start_tick": 0, "duration_ticks": TICKS}]},
        ])
        code, out, _ = self.render(m, "--target-bitrate", "4000000", "--queue-blocks", "3")
        self.assertEqual(code, 0)
        f = self.fields(out)
        self.assertEqual(f["target_bitrate"], "4000000")
        self.assertGreater(int(f["frames"]), 0)
        for key in ("bitrate_error_pct", "rms_dbfs", "clipping_pct",
                    "manifest_alignment_ms", "backpressure_wait_ms"):
            self.assertTrue(np.isfinite(float(f[key])), key)
        manifest = json.loads((self.out / "t.manifest.json").read_text())
        self.assertEqual(manifest["video"]["frames"], 30)
        self.assertEqual(len(manifest["video"]["frame_sha256"]), 30)
        self.assertNotIn(str(self.dir), json.dumps(manifest))  # no absolute paths

    def test_determinism(self) -> None:
        write_tone(self.dir / "a.wav", RATE, 1.0, 440.0, 2)
        m = make_manifest(self.dir, [one_track()])
        code1, _, _ = self.render(m)
        first = json.loads((self.out / "t.manifest.json").read_text())
        code2, _, _ = self.render(m)
        second = json.loads((self.out / "t.manifest.json").read_text())
        self.assertEqual((code1, code2), (0, 0))
        first.pop("backpressure_wait_ms")
        second.pop("backpressure_wait_ms")
        self.assertEqual(first, second)

    def test_zero_length_track_and_short_final_block(self) -> None:
        write_tone(self.dir / "a.wav", RATE, 1.0, 440.0, 2)
        write_tone(self.dir / "empty.wav", RATE, 0.0, 0.0, 2)
        m = make_manifest(self.dir, [
            one_track(),
            {"id": "z", "path": "empty.wav", "offset_ticks": 0,
             "segments": [{"start_tick": 0, "duration_ticks": 0}]},
        ])
        code, out, _ = self.render(m)
        self.assertEqual(code, 0)
        self.assertGreater(int(self.fields(out)["frames"]), 0)


class ManifestValidationTests(StageflowTestBase):
    def expect_manifest_error(self, tracks: list[dict], code: str, **kw) -> None:
        m = make_manifest(self.dir, tracks, **kw)
        exit_code, _, err = self.render(m)
        self.assertEqual(exit_code, 2)
        self.assertIn(f"error_code={code}", err)

    def test_negative_duration(self) -> None:
        self.expect_manifest_error([one_track(seg_ticks=-1)], "MANIFEST_NEGATIVE_DURATION")

    def test_unknown_track(self) -> None:
        m = make_manifest(self.dir, [one_track()])
        doc = json.loads(m.read_text())
        doc["delivery"]["tracks"] = ["a", "ghost"]
        m.write_text(json.dumps(doc))
        code, _, err = self.render(m)
        self.assertEqual(code, 2)
        self.assertIn("error_code=MANIFEST_UNKNOWN_TRACK", err)

    def test_out_of_range_segment(self) -> None:
        self.expect_manifest_error([one_track(seg_ticks=TICKS + 1)], "MANIFEST_RANGE")

    def test_nonmonotonic_cuts(self) -> None:
        self.expect_manifest_error(
            [one_track(cut_ticks=[MASTER_RATE // 2, MASTER_RATE // 4])],
            "MANIFEST_NONMONOTONIC",
        )


class CorruptionTests(StageflowTestBase):
    def make_truncated(self) -> Path:
        write_tone(self.dir / "a.wav", RATE, 1.0, 440.0, 2)
        data = (self.dir / "a.wav").read_bytes()
        (self.dir / "a.wav").write_bytes(data[: int(len(data) * 0.7)])
        return make_manifest(self.dir, [one_track()])

    def test_truncated_wav_reports_location(self) -> None:
        code, _, err = self.render(self.make_truncated())
        self.assertEqual(code, 3)
        f = self.fields(err)
        self.assertEqual(f["error_code"], "WAV_TRUNCATED")
        self.assertEqual(f["track"], "a")
        self.assertEqual(f["path"], "a.wav")
        self.assertIn("sample_offset", f)

    def test_truncated_wav_mute_policy(self) -> None:
        code, out, _ = self.render(self.make_truncated(), "--on-corrupt", "mute")
        self.assertEqual(code, 0)
        self.assertGreater(int(self.fields(out)["missing_blocks"]), 0)
        manifest = json.loads((self.out / "t.manifest.json").read_text())
        self.assertTrue(manifest["muted_ranges"])

    def test_header_corrupt(self) -> None:
        (self.dir / "a.wav").write_bytes(b"not a wav file at all")
        code, _, err = self.render(make_manifest(self.dir, [one_track()]))
        self.assertEqual(code, 3)
        self.assertEqual(self.fields(err)["error_code"], "WAV_HEADER_CORRUPT")

    def test_nonfinite_samples(self) -> None:
        data = np.zeros((RATE // 2, 2), np.float32)
        data[12345, 0] = np.nan
        sf.write(self.dir / "a.wav", data, RATE, subtype="FLOAT")
        m = make_manifest(self.dir, [one_track(seg_ticks=TICKS // 2)], duration_ticks=TICKS // 2)
        code, _, err = self.render(m)
        self.assertEqual(code, 3)
        f = self.fields(err)
        self.assertEqual(f["error_code"], "NONFINITE_SAMPLE")
        self.assertIn("sample_offset", f)


class BackpressureTests(StageflowTestBase):
    def test_slow_consumer_backpressure(self) -> None:
        write_tone(self.dir / "a.wav", RATE, 1.0, 440.0, 2)
        m = make_manifest(self.dir, [one_track()])
        code, out, _ = self.render(
            m, "--queue-blocks", "2", "--consumer-delay-ms", "100",
            "--stall-threshold-ms", "50",
        )
        self.assertEqual(code, 0)
        self.assertGreater(float(self.fields(out)["backpressure_wait_ms"]), 0.0)


class CancelResumeTests(StageflowTestBase):
    def test_cancel_then_resume(self) -> None:
        write_tone(self.dir / "a.wav", RATE, 1.0, 440.0, 2)
        m = make_manifest(self.dir, [one_track()])
        code, _, err = self.render(m, "--cancel-after-blocks", "3")
        self.assertEqual(code, 4)
        self.assertIn("error_code=INTERRUPTED", err)
        self.assertTrue((self.out / ".work-t" / "checkpoint.json").exists())
        code, out, _ = self.render(m, "--resume")
        self.assertEqual(code, 0)
        self.assertGreater(int(self.fields(out)["frames"]), 0)
        self.assertFalse((self.out / ".work-t").exists())  # temp cleaned
        manifest = json.loads((self.out / "t.manifest.json").read_text())
        self.assertEqual(manifest["resumed_from_block"], 3)


if __name__ == "__main__":
    unittest.main()
