"""Acceptance-oriented tests for the CLI, summary fields and exit codes."""

from __future__ import annotations

import io
import json
import os
import unittest
from contextlib import redirect_stdout

from avtimeline.cli import main

try:  # both `unittest discover -s tests` and package imports
    from .support import TempDirMixin
except ImportError:  # pragma: no cover - discovery from the tests directory
    from support import TempDirMixin


class DemoSummaryTests(TempDirMixin, unittest.TestCase):
    def _demo(self, *extra: str) -> tuple[int, dict]:
        workdir = self.path("demo")
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = main(
                [
                    "demo",
                    "--workdir",
                    workdir,
                    "--break-at",
                    "encode",
                    "--resume",
                    "--corrupt-sample",
                    "--edge-sample",
                    "--clean",
                    *extra,
                ]
            )
        return code, json.loads(buffer.getvalue())

    def test_acceptance_demo_fields(self) -> None:
        code, summary = self._demo()
        self.assertEqual(code, 0)
        self.assertEqual(summary["first_error_track"], "corrupt_cam")
        self.assertEqual(summary["first_error_pts"], "6/5")
        self.assertAlmostEqual(summary["first_error_pts_seconds"], 1.2, places=6)
        self.assertGreaterEqual(summary["aligned_segments"], 6)
        self.assertEqual(summary["boundary_error"], "0")
        self.assertTrue(summary["sha256_match"])
        self.assertLess(summary["resume_ratio"], 0.5)
        self.assertTrue(summary["resume_within_budget"])
        self.assertEqual(summary["interrupted_at"], "encode")
        self.assertEqual(summary["checkpoint_status"], "valid")
        self.assertEqual(summary["exit_code"], 0)

    def test_summary_has_no_media_payload(self) -> None:
        _, summary = self._demo()
        text = json.dumps(summary)
        for forbidden in ("base64", "data:", "\\x00"):
            self.assertNotIn(forbidden, text)
        self.assertLess(len(text), 200_000)

    def test_artifact_files_exist(self) -> None:
        _, summary = self._demo()
        workdir = summary["workdir"]
        for name in ("manifest.json", "checkpoint.json", "summary.json", "timeline.json"):
            self.assertTrue(os.path.exists(os.path.join(workdir, name)), name)
        with open(os.path.join(workdir, "manifest.json"), encoding="utf-8") as handle:
            manifest = json.load(handle)
        self.assertEqual(manifest["stats"]["segment_count"], summary["segment_count"])
        for segment in manifest["segments"]:
            for entry in segment["renditions"].values():
                self.assertEqual(len(entry["sha256"]), 64)


class ExitCodeTests(TempDirMixin, unittest.TestCase):
    def test_invalid_timeline_returns_two(self) -> None:
        path = self.path("bad.json")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write('{"tracks": []}')
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = main(["inspect", path])
        self.assertEqual(code, 2)

    def test_inspect_valid_timeline_returns_zero(self) -> None:
        path = self.path("ok.json")
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(
                {
                    "window": {"start": 0, "end": 1},
                    "tracks": [
                        {
                            "id": "cam",
                            "clips": [{"id": "a", "start": 0, "end": 1}],
                        }
                    ],
                },
                handle,
            )
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = main(["inspect", path])
        payload = json.loads(buffer.getvalue())
        self.assertEqual(code, 0)
        self.assertTrue(payload["valid"])

    def test_interrupted_without_resume_is_incomplete(self) -> None:
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = main(
                [
                    "demo",
                    "--workdir",
                    self.path("partial"),
                    "--break-at",
                    "encode",
                    "--clean",
                ]
            )
        payload = json.loads(buffer.getvalue())
        self.assertEqual(code, 3)
        self.assertTrue(payload["interrupted"])
        self.assertFalse(payload["resume_requested"])

    def test_stop_policy_on_damaged_material_returns_two(self) -> None:
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = main(
                [
                    "demo",
                    "--workdir",
                    self.path("stop"),
                    "--corrupt-sample",
                    "--policy",
                    "stop",
                    "--clean",
                ]
            )
        payload = json.loads(buffer.getvalue())
        self.assertEqual(code, 2)
        self.assertEqual(payload["exit_code"], 2)
        self.assertEqual(payload["first_error_track"], "corrupt_cam")

    def test_verify_command_is_reproducible(self) -> None:
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = main(["verify", "--workdir", self.path("verify")])
        payload = json.loads(buffer.getvalue())
        self.assertEqual(code, 0)
        self.assertTrue(payload["sha256_match"])
        self.assertGreaterEqual(payload["aligned_segments"], 6)

    def test_bad_stage_name_rejected(self) -> None:
        with self.assertRaises(SystemExit):
            main(["demo", "--workdir", self.path("x"), "--break-at", "nope"])


class SlowResumeGuardTests(TempDirMixin, unittest.TestCase):
    def test_resume_budget_flag_reflects_measurement(self) -> None:
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = main(
                [
                    "demo",
                    "--workdir",
                    self.path("demo"),
                    "--break-at",
                    "normalize",
                    "--resume",
                    "--clean",
                ]
            )
        payload = json.loads(buffer.getvalue())
        self.assertEqual(code, 0)
        self.assertTrue(payload["resume_within_budget"])
        self.assertLess(payload["resume_seconds"], payload["full_seconds"])


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
