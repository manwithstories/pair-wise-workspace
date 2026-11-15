"""端到端一致性与错误链路测试（标准库 unittest，无外部依赖）。"""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np
import soundfile as sf

from mixctl.automation import Automation
from mixctl.cli import main
from mixctl.errors import (
    EXIT_AUDIO_IO,
    EXIT_DIVERGENCE,
    EXIT_GRAPH,
    EXIT_OK,
    EXIT_USAGE,
)
from mixctl.fixtures import generate_fixtures
from mixctl.graph import build_graph
from mixctl.metrics import Metrics
from mixctl.project import Keyframe, load_project, parse_project
from mixctl.resample import Resampler, rational_ratio

REPO = Path(__file__).resolve().parents[1]


class CliRun:
    def __init__(self, root: Path):
        self.root = root
        self.project = root / "demo.mix.json"

    def run(self, *args: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, "-m", "mixctl", *args],
            cwd=REPO, capture_output=True, text=True,
        )


class PipelineTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.fixture_dir = self.tmp / "fixtures"
        generate_fixtures(self.fixture_dir)
        self.cli = CliRun(self.fixture_dir)
        self.mix_wav = self.tmp / "mix.wav"
        self.stream_wav = self.tmp / "stream.wav"
        self.report = self.tmp / "report.json"

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _render(self, *extra: str) -> subprocess.CompletedProcess:
        return self.cli.run(
            "render", "--project", str(self.cli.project),
            "--out", str(self.mix_wav), "--report", str(self.report), *extra,
        )

    def test_fixtures_and_report_shape(self):
        result = self._render()
        self.assertEqual(result.returncode, EXIT_OK, result.stderr)
        report = json.loads(self.report.read_text())
        self.assertEqual(report["target_sr"], 48000)
        self.assertEqual(len(report["tracks"]), 5)
        expected_track_keys = {
            "name", "source_sr", "target_sr", "ratio",
            "peak_dbfs", "rms_dbfs", "clip_samples", "auto_points",
        }
        for track in report["tracks"]:
            self.assertEqual(set(track.keys()), expected_track_keys)
        self.assertEqual(
            set(report["master"].keys()),
            {"peak_dbfs", "rms_dbfs", "clip_samples", "auto_points"},
        )
        self.assertEqual(
            set(report["automation"].keys()),
            {"hop", "samples", "first_time", "last_time", "first_db", "last_db"},
        )
        ambience = next(t for t in report["tracks"] if t["name"] == "ambience")
        self.assertIsNone(ambience["rms_dbfs"])
        self.assertIsNone(ambience["peak_dbfs"])

    def test_rational_ratio(self):
        self.assertEqual(rational_ratio(44100, 48000), (160, 147))
        self.assertEqual(rational_ratio(48000, 96000), (2, 1))
        self.assertEqual(rational_ratio(96000, 48000), (1, 2))
        with self.assertRaises(ValueError):
            rational_ratio(0, 48000)

    def test_resampler_block_invariance(self):
        sample_rate = 44100
        timeline = np.arange(sample_rate) / sample_rate
        source = (0.5 * np.sin(2 * np.pi * 440 * timeline)).astype(np.float64)
        whole = Resampler(sample_rate, 48000)
        whole.push(source)
        whole.mark_eof()
        expected = whole.flush()
        for chunk in (1, 100, 777, 7777):
            stream = Resampler(sample_rate, 48000)
            for start in range(0, source.shape[0], chunk):
                stream.push(source[start:start + chunk])
            stream.mark_eof()
            got = stream.flush()
            self.assertTrue(
                np.array_equal(expected, got),
                f"resampler diverged at chunk={chunk}",
            )

    def test_automation_block_invariance(self):
        frames = [
            Keyframe(0.0, -6.0), Keyframe(10.0, -1.0), Keyframe(30.0, -1.0),
            Keyframe(45.0, -4.0), Keyframe(60.0, -6.0),
        ]
        total = 48000 * 5
        for hop in (1, 32, 64, 100):
            curve = Automation(frames, 48000, hop)
            full = curve.gains(0, total)
            for chunk in (1, 137, 777):
                joined = np.concatenate([
                    curve.gains(start, min(chunk, total - start))
                    for start in range(0, total, chunk)
                ])
                self.assertTrue(np.array_equal(full, joined), (hop, chunk))

    def test_dual_path_identical_all_chunks(self):
        offline = self._render()
        self.assertEqual(offline.returncode, EXIT_OK, offline.stderr)
        for chunk in (100, 777, 7777):
            stream = self.cli.run(
                "render", "--project", str(self.cli.project), "--engine", "stream",
                "--chunk", str(chunk), "--out", str(self.stream_wav),
            )
            self.assertEqual(stream.returncode, EXIT_OK, stream.stderr)
            compare = self.cli.run(
                "compare", "--a", str(self.mix_wav), "--b", str(self.stream_wav),
                "--json",
            )
            self.assertEqual(compare.returncode, EXIT_OK, compare.stderr)
            payload = json.loads(compare.stdout.strip())
            self.assertTrue(payload["identical"])
            self.assertEqual(payload["max_abs_diff"], 0.0)
            self.assertGreater(payload["samples"], 0)

    def test_stream_memory_bounded(self):
        # 按引擎真实的按需投喂逻辑跑 10 分钟，缓冲必须有上界。
        rng = np.random.default_rng(0)
        source = rng.standard_normal(44100 * 600)
        resampler = Resampler(44100, 48000)
        pos = 0
        eof = False
        max_buffer = 0
        target_total = 48000 * 600
        produced = 0
        while produced < target_total:
            frames = min(777, target_total - produced)
            need = resampler.needed_input_for(frames)
            while not eof and resampler._pushed_total < need:
                take = min(65536, source.shape[0] - pos)
                if take <= 0:
                    eof = True
                    resampler.mark_eof()
                    break
                resampler.push(source[pos:pos + take])
                pos += take
            produced += resampler.read(frames).shape[0]
            max_buffer = max(max_buffer, resampler._buffer.shape[0])
        self.assertLess(max_buffer, 100000)

    def test_compare_mismatch_exit_5(self):
        self.assertEqual(self._render().returncode, EXIT_OK)
        other_wav = self.tmp / "other.wav"
        data, sample_rate = sf.read(self.mix_wav, dtype="float64", always_2d=True)
        sf.write(str(other_wav), data * 0.5, sample_rate, subtype="PCM_24")
        result = self.cli.run(
            "compare", "--a", str(self.mix_wav), "--b", str(other_wav), "--json",
        )
        self.assertEqual(result.returncode, EXIT_DIVERGENCE)
        self.assertFalse(json.loads(result.stdout.strip())["identical"])

    def test_clip_detection_end_to_end(self):
        # 构造超过满幅的单声道源，归零增益与自动化，验证削波统计与钳制。
        wav_path = self.fixture_dir / "wav" / "dialogue.wav"
        sample_rate = 44100
        timeline = np.arange(sample_rate) / sample_rate
        over = (1.2 * np.sin(2 * np.pi * 300 * timeline)).astype(np.float64)
        sf.write(str(wav_path), over, sample_rate, subtype="FLOAT")
        project = json.loads(self.cli.project.read_text())
        for track in project["tracks"]:
            track["gain_db"] = 0.0
            track["automation"] = [{"time": 0.0, "db": 0.0}]
        self.cli.project.write_text(json.dumps(project))
        result = self._render()
        self.assertEqual(result.returncode, EXIT_OK, result.stderr)
        report = json.loads(self.report.read_text())
        dialogue = next(t for t in report["tracks"] if t["name"] == "dialogue")
        self.assertGreater(dialogue["clip_samples"], 0)
        data, _ = sf.read(self.mix_wav, dtype="float64", always_2d=True)
        self.assertLessEqual(np.abs(data).max(), 1.0)

    def test_output_is_24bit_and_clamped(self):
        self.assertEqual(self._render().returncode, EXIT_OK)
        info = sf.info(str(self.mix_wav))
        self.assertEqual(info.subtype, "PCM_24")
        data, _ = sf.read(self.mix_wav, dtype="float64", always_2d=True)
        self.assertLessEqual(np.abs(data).max(), 1.0)

    def test_report_byte_reproducible(self):
        self._render()
        first = self.report.read_bytes()
        second_report = self.tmp / "report2.json"
        self.cli.run(
            "render", "--project", str(self.cli.project),
            "--out", str(self.tmp / "mix2.wav"), "--report", str(second_report),
        )
        self.assertEqual(first, second_report.read_bytes())

    def test_metrics_clip_and_silence(self):
        metrics = Metrics(1.0)
        metrics.update(np.array([0.5, -1.0, 1.0, 0.25]))
        report = metrics.to_report()
        self.assertEqual(report["clip_samples"], 2)
        self.assertEqual(report["peak_dbfs"], 0.0)
        silent = Metrics()
        silent.update(np.zeros(8))
        self.assertIsNone(silent.to_report()["rms_dbfs"])
