"""CLI 端到端与输出失败测试:子进程验证真实退出码契约。

退出码:0 成功 / 2 参数配置 / 3 媒体 / 4 输出 / 5 不一致。
"""

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np
import soundfile as sf

from mixer.offline import write_report, write_wav
from mixer.types import OutputError, StatsResult

ROOT = Path(__file__).resolve().parent.parent


class OutputFailureTest(unittest.TestCase):
    def test_unwritable_wav_path(self):
        with tempfile.TemporaryDirectory() as d:
            blocker = Path(d) / "blocker"
            blocker.write_text("not a directory")
            with self.assertRaises(OutputError):
                write_wav(blocker / "x.wav", np.zeros((4, 2)), 48000)

    def test_unwritable_report_path(self):
        with tempfile.TemporaryDirectory() as d:
            blocker = Path(d) / "blocker"
            blocker.write_text("not a directory")
            stats = StatsResult(
                sample_rate=48000, channels=2, frames=1, tracks=1,
                automation_samples=0, peak_dbfs=-1.0, rms_dbfs=-1.0,
                effective_rms_dbfs=-1.0, clipped_samples=0,
                clip_threshold=1.0, mode="offline", block_frames=64,
                error_policy="fail_fast",
            )
            with self.assertRaises(OutputError):
                write_report(blocker / "r.json", stats)


class CliExitCodeTest(unittest.TestCase):
    def _run(self, *args):
        return subprocess.run(
            [sys.executable, str(ROOT / "cli.py"), *args],
            capture_output=True, text=True, cwd=str(ROOT),
        )

    def test_generate_mix_verify_demo_flow(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertEqual(self._run("generate", "--out", d).returncode, 0)
            r1 = self._run("mix", "--config", f"{d}/project.json",
                           "--output", f"{d}/off.wav", "--report", f"{d}/off.json",
                           "--mode", "offline")
            self.assertEqual(r1.returncode, 0, r1.stderr)
            r2 = self._run("mix", "--config", f"{d}/project.json",
                           "--output", f"{d}/stm.wav", "--report", f"{d}/stm.json",
                           "--mode", "stream", "--block-frames", "1024")
            self.assertEqual(r2.returncode, 0, r2.stderr)
            r3 = self._run("verify", "--left", f"{d}/off.wav",
                           "--right", f"{d}/stm.wav", "--report", f"{d}/v.json")
            self.assertEqual(r3.returncode, 0, r3.stderr)
            report = json.loads(Path(f"{d}/v.json").read_text())
            self.assertLessEqual(report["max_abs_diff"], 1e-6)
            mix_report = json.loads(Path(f"{d}/off.json").read_text())
            self.assertEqual(mix_report["sample_rate"], 48000)
            self.assertEqual(mix_report["channels"], 2)
            self.assertEqual(mix_report["frames"], 240000)
            self.assertEqual(mix_report["tracks"], 4)
            self.assertEqual(mix_report["automation_samples"], 20)
            self.assertIn("peak_dbfs", mix_report)
            self.assertIn("effective_rms_dbfs", mix_report)

    def test_arg_error_exit_2(self):
        self.assertEqual(self._run("mix", "--mode", "bogus").returncode, 2)

    def test_missing_config_exit_2(self):
        r = self._run("mix", "--config", "/no/such/p.json",
                      "--output", "o.wav", "--report", "r.json", "--mode", "offline")
        self.assertEqual(r.returncode, 2)

    def test_media_error_exit_3(self):
        with tempfile.TemporaryDirectory() as d:
            cfg = Path(d) / "p.json"
            cfg.write_text(json.dumps({
                "sample_rate": 48000, "duration_frames": 10,
                "tracks": [{"name": "x", "path": "nope.wav"}],
            }))
            r = self._run("mix", "--config", str(cfg),
                          "--output", f"{d}/o.wav", "--report", f"{d}/r.json",
                          "--mode", "offline")
            self.assertEqual(r.returncode, 3)

    def test_output_error_exit_4(self):
        with tempfile.TemporaryDirectory() as d:
            blocker = Path(d) / "file"
            blocker.write_text("x")
            self.assertEqual(self._run("generate", "--out", d).returncode, 0)
            r = self._run("mix", "--config", f"{d}/project.json",
                          "--output", f"{blocker}/o.wav",
                          "--report", f"{d}/r.json", "--mode", "offline")
            self.assertEqual(r.returncode, 4)

    def test_inconsistency_exit_5(self):
        with tempfile.TemporaryDirectory() as d:
            sf.write(f"{d}/a.wav", np.zeros((10, 2)), 48000, subtype="FLOAT")
            sf.write(f"{d}/b.wav", np.ones((10, 2)), 48000, subtype="FLOAT")
            r = self._run("verify", "--left", f"{d}/a.wav",
                          "--right", f"{d}/b.wav", "--report", f"{d}/v.json")
            self.assertEqual(r.returncode, 5)

    def test_shape_mismatch_exit_5(self):
        with tempfile.TemporaryDirectory() as d:
            sf.write(f"{d}/a.wav", np.zeros((10, 2)), 48000, subtype="FLOAT")
            sf.write(f"{d}/b.wav", np.zeros((8, 2)), 48000, subtype="FLOAT")
            r = self._run("verify", "--left", f"{d}/a.wav",
                          "--right", f"{d}/b.wav", "--report", f"{d}/v.json")
            self.assertEqual(r.returncode, 5)
            self.assertEqual(
                json.loads(Path(f"{d}/v.json").read_text())["reason"],
                "shape_mismatch",
            )


if __name__ == "__main__":
    unittest.main()
