"""校验与异常恢复测试: 错误定位、错误类型、可继续条目数与 CLI 退出码。"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from fractions import Fraction
from pathlib import Path

from compositor.timeline import parse_timeline
from fixtures.generate import build_fixture


def _minimal_timeline() -> dict:
    raw = build_fixture()
    # 去掉多余条目, 保留最小可渲染结构, 各用例按需破坏。
    raw["layers"] = [dict(raw["layers"][0])]
    raw["audio_events"] = []
    return raw


class ValidationErrorTests(unittest.TestCase):
    def _error_types(self, raw):
        _, report = parse_timeline(raw)
        return {(e.location, e.error_type.value) for e in report.errors}

    def test_missing_field_reports_location(self):
        raw = _minimal_timeline()
        del raw["layers"][0]["source_id"]
        errors = self._error_types(raw)
        self.assertTrue(any(loc == "layers[0].source_id"
                            for loc, _ in errors))
        self.assertTrue(any(typ == "missing_field" for _, typ in errors))

    def test_alpha_out_of_range(self):
        raw = _minimal_timeline()
        raw["layers"][0]["alpha"] = 1.5
        self.assertIn(("layers[0].alpha", "alpha_out_of_range"),
                      self._error_types(raw))
        raw["layers"][0]["alpha"] = -0.2
        self.assertIn(("layers[0].alpha", "alpha_out_of_range"),
                      self._error_types(raw))

    def test_negative_and_inverted_duration(self):
        raw = _minimal_timeline()
        raw["layers"][0].update(frame_in=10, frame_out=10)
        self.assertIn(("layers[0]", "negative_duration"),
                      self._error_types(raw))
        raw["layers"][0].update(frame_in=50, frame_out=10)
        self.assertIn(("layers[0]", "negative_duration"),
                      self._error_types(raw))

    def test_frame_out_of_range(self):
        raw = _minimal_timeline()
        raw["layers"][0].update(frame_in=0, frame_out=999)
        self.assertIn(("layers[0]", "frame_out_of_range"),
                      self._error_types(raw))

    def test_non_monotonic_audio_timecode(self):
        raw = _minimal_timeline()
        raw["audio_events"] = [
            {"id": "a", "start": "1/10", "duration": "1/10"},
            {"id": "b", "start": "1/30", "duration": "1/10"},
        ]
        self.assertIn(("audio_events[1].start", "non_monotonic_time"),
                      self._error_types(raw))

    def test_unknown_blend_enters_error_chain(self):
        raw = _minimal_timeline()
        raw["layers"][0]["blend"] = "photoshop-dissolve"
        self.assertIn(("layers[0].blend", "unknown_blend_mode"),
                      self._error_types(raw))

    def test_size_mismatch_mask(self):
        raw = _minimal_timeline()
        raw["sources"].append({
            "id": "bad_mask", "kind": "mask",
            "width": 10, "height": 10,
            "spec": {"rx_ratio": 0.5, "ry_ratio": 0.5},
        })
        raw["layers"][0]["mask_source_id"] = "bad_mask"
        self.assertIn(("layers[0]", "size_mismatch"),
                      self._error_types(raw))

    def test_corrupt_json_is_located(self):
        from compositor.timeline import load_timeline

        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "broken.json"
            path.write_text("{not valid json", encoding="utf-8")
            timeline, report = load_timeline(path)
        self.assertIsNone(timeline)
        self.assertEqual(report.errors[0].error_type.value, "json_corrupt")
        self.assertTrue(report.errors[0].location)

    def test_recoverable_entry_count(self):
        raw = _minimal_timeline()
        # 一条坏图层 + 一条好图层; 好素材仍计入可继续条目。
        good = dict(raw["layers"][0])
        good["id"] = "good_layer"
        bad = dict(raw["layers"][0])
        bad["id"] = "bad_layer"
        bad["alpha"] = 2.0
        raw["layers"] = [good, bad]
        _, report = parse_timeline(raw)
        self.assertFalse(report.to_dict()["ok"])
        self.assertEqual(report.valid_layers, 1)
        self.assertEqual(report.valid_sources, 5)
        self.assertEqual(report.recoverable_entries, 6)

    def test_bad_fps_rejected(self):
        raw = _minimal_timeline()
        raw["timeline"]["fps"] = 25
        _, report = parse_timeline(raw)
        self.assertTrue(any(typ == "bad_value"
                            for _, typ in self._error_types(raw)))
        self.assertIn("timeline.fps",
                      [e.location for e in report.errors])

    def test_rational_string_and_override_rate(self):
        raw = build_fixture()
        timeline, report = parse_timeline(raw, fps_override=24,
                                          sample_rate_override=44100)
        self.assertTrue(report.to_dict()["ok"])
        self.assertEqual(timeline.fps, 24)
        self.assertEqual(timeline.sample_rate, 44100)
        self.assertEqual(timeline.audio_events[1].start, Fraction(1, 60))


class CliExitCodeTests(unittest.TestCase):
    def _run(self, *args):
        return subprocess.run(
            [sys.executable, "-m", "compositor.cli", *args],
            capture_output=True, text=True, cwd=Path(__file__).parents[1])

    def test_validation_failure_exit_code_2(self):
        raw = _minimal_timeline()
        raw["layers"][0]["alpha"] = 3.0
        with tempfile.TemporaryDirectory() as tmp:
            tl = Path(tmp) / "bad.json"
            tl.write_text(json.dumps(raw), encoding="utf-8")
            result = self._run("render", "--timeline", str(tl),
                               "--output", str(Path(tmp) / "o.jsonl"))
        self.assertEqual(result.returncode, 2)
        payload = json.loads(result.stdout.strip())
        self.assertFalse(payload["ok"])
        self.assertEqual(payload["error"]["code"], "validation_failed")
        self.assertIn("errors", payload["error"]["details"])

    def test_digest_write_failure_exit_code_4(self):
        raw = build_fixture()
        with tempfile.TemporaryDirectory() as tmp:
            tl = Path(tmp) / "ok.json"
            tl.write_text(json.dumps(raw), encoding="utf-8")
            # 输出到一个不存在的目录下的非法嵌套路径(文件占位目录)。
            blocker = Path(tmp) / "blocker"
            blocker.write_text("x", encoding="utf-8")
            result = self._run("render", "--timeline", str(tl),
                               "--output", str(blocker / "o.jsonl"))
        self.assertEqual(result.returncode, 4)
        self.assertEqual(json.loads(result.stdout.strip())
                         ["error"]["code"], "digest_write_failed")

    def test_happy_path_exit_code_0(self):
        raw = build_fixture()
        with tempfile.TemporaryDirectory() as tmp:
            tl = Path(tmp) / "tl.json"
            tl.write_text(json.dumps(raw), encoding="utf-8")
            out = Path(tmp) / "out.jsonl"
            result = self._run("render", "--timeline", str(tl),
                               "--fps", "30", "--sample-rate", "48000",
                               "--output", str(out))
            self.assertEqual(result.returncode, 0, result.stderr)
            payload = json.loads(result.stdout.strip())
            self.assertEqual(payload["data"]["frame_count"], 120)
            self.assertIn("summary_sha256", payload["data"])


if __name__ == "__main__":
    unittest.main()
