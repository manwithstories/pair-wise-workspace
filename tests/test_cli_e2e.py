"""End-to-end tests: CLI subcommands, exit codes, determinism."""
from __future__ import annotations

import contextlib
import io
import json
import subprocess
import sys
import unittest
import wave
from pathlib import Path
from tempfile import TemporaryDirectory

from audio_slicer import cli

REPO_ROOT = Path(__file__).resolve().parent.parent


def run_cli(argv):
    with contextlib.redirect_stdout(io.StringIO()):
        return cli.main(argv)


class CliE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = TemporaryDirectory()
        cls.root = Path(cls.tmp.name)
        cls.fixtures = cls.root / "fixtures"
        subprocess.run(
            [sys.executable, str(REPO_ROOT / "scripts" / "make_fixtures.py"),
             "--out", str(cls.fixtures)], check=True,
            capture_output=True)
        cls.db = cls.root / "index.db"

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def _analyze(self, json_name="analyze.json"):
        out = self.root / json_name
        rc = run_cli(["analyze", "--input",
                      str(self.fixtures / "pack_noisy_fade.wav"),
                      "--db", str(self.db), "--json", str(out),
                      "--frame-ms", "10", "--min-silence-ms", "180",
                      "--fade-guard-ms", "30"])
        self.assertEqual(rc, 0)
        return json.loads(out.read_text(encoding="utf-8"))

    def test_analyze_output_shape(self):
        data = self._analyze()
        self.assertEqual(data["run_id"], 1)
        self.assertTrue(10 <= data["segment_count"] <= 60)
        ratio = data["silence_total_samples"] / data["total_samples"]
        self.assertTrue(0.2 <= ratio <= 0.6)
        self.assertAlmostEqual(
            data["threshold_dbfs"] - data["noise_floor_dbfs"], 8.0, places=4)
        first = data["segments"][0]
        for key in ("ordinal", "start_sample", "end_sample",
                    "fade_in_samples", "fade_out_samples"):
            self.assertIn(key, first)
        sil = data["silences"][0]
        self.assertEqual(sil["end_sample"] - sil["start_sample"],
                         sil["duration_samples"])

    def test_determinism_byte_identical(self):
        first = self._analyze("det_a.json")
        second = self._analyze("det_b.json")
        self.assertEqual((self.root / "det_a.json").read_bytes(),
                         (self.root / "det_b.json").read_bytes())
        self.assertEqual(first["run_id"], second["run_id"])

    def test_batch_report_export(self):
        batch_json = self.root / "batch.json"
        rc = run_cli(["batch", "--input-dir", str(self.fixtures / "batch"),
                      "--db", str(self.db),
                      "--out-dir", str(self.root / "segments"),
                      "--json", str(batch_json), "--strict"])
        self.assertEqual(rc, 0)
        summary = json.loads(batch_json.read_text(encoding="utf-8"))
        self.assertEqual(summary["files_failed"], 0)
        self.assertEqual(summary["files_ok"], 4)
        self.assertGreater(summary["segments_total"], 0)
        rc = run_cli(["report", "--db", str(self.db), "--run-id", "1",
                      "--format", "text"])
        self.assertEqual(rc, 0)
        rc = run_cli(["export", "--db", str(self.db), "--run-id", "1",
                      "--out-dir", str(self.root / "segments")])
        self.assertEqual(rc, 0)
        manifest = json.loads((self.root / "segments" / "manifest.json")
                              .read_text(encoding="utf-8"))
        self.assertEqual(manifest["file_count"], 18)
        exported = sorted((self.root / "segments").glob("pack_noisy_fade_seg*.wav"))
        self.assertEqual(len(exported), 18)
        self.assertEqual(exported[0].name, "pack_noisy_fade_seg001.wav")
        with wave.open(str(exported[0]), "rb") as handle:
            self.assertEqual(handle.getnchannels(), 1)
            self.assertEqual(handle.getsampwidth(), 2)

    def test_exit_codes(self):
        self.assertEqual(run_cli(["analyze", "--input",
                                  str(self.root / "missing.wav")]), 2)
        not_wav = self.root / "note.txt"
        not_wav.write_text("hello")
        self.assertEqual(run_cli(["analyze", "--input", str(not_wav)]), 2)
        eight_bit = self.root / "eight.wav"
        with wave.open(str(eight_bit), "wb") as handle:
            handle.setnchannels(1)
            handle.setsampwidth(1)
            handle.setframerate(48000)
            handle.writeframes(b"\x80" * 4800)
        self.assertEqual(run_cli(["analyze", "--input", str(eight_bit)]), 2)
        self.assertEqual(run_cli(["report", "--db", str(self.db),
                                  "--run-id", "999"]), 2)
        self.assertEqual(run_cli(["report", "--db", str(self.db),
                                  "--run-id", "1", "--margin-ms", "100000",
                                  "--strict"]), 3)
        self.assertEqual(run_cli(["report", "--db",
                                  str(self.fixtures),
                                  "--run-id", "1"]), 4)

    def test_selftest_subprocess(self):
        out_dir = self.root / "selftest_out"
        proc = subprocess.run(
            [sys.executable, "-m", "audio_slicer", "selftest",
             "--fixtures", str(self.root / "st_fixtures"),
             "--out-dir", str(out_dir)],
            cwd=REPO_ROOT, capture_output=True, text=True)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("SELFTEST PASS", proc.stdout)


if __name__ == "__main__":
    unittest.main()
