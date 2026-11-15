"""工程校验、混音图与音频 IO 的错误链路测试。"""

from __future__ import annotations

import json
import math
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np
import soundfile as sf

from mixctl.errors import (
    EXIT_AUDIO_IO,
    EXIT_GRAPH,
    EXIT_OK,
    EXIT_USAGE,
)
from mixctl.fixtures import generate_fixtures
from mixctl.graph import build_graph
from mixctl.project import ProjectError, load_project, parse_project

REPO = Path(__file__).resolve().parents[1]


def base_project() -> dict:
    return {
        "target_sr": 48000,
        "duration": 1.0,
        "hop": 64,
        "master_bus": "master",
        "buses": [
            {"name": "sub", "output": "master"},
            {"name": "master", "output": None},
        ],
        "tracks": [
            {
                "name": "t1", "file": "a.wav", "channels": 1, "bus": "sub",
                "gain_db": 0.0, "muted": False,
                "automation": [{"time": 0.0, "db": -1.0}],
            }
        ],
    }


class ValidationTest(unittest.TestCase):
    def test_duplicate_track_name(self):
        raw = base_project()
        raw["tracks"].append(dict(raw["tracks"][0], name="t1"))
        with self.assertRaises(ProjectError) as ctx:
            parse_project(raw)
        self.assertIn("tracks[1].name", str(ctx.exception))

    def test_unknown_bus(self):
        raw = base_project()
        raw["tracks"][0]["bus"] = "ghost"
        with self.assertRaises(ProjectError) as ctx:
            parse_project(raw)
        self.assertIn("tracks[0].bus", str(ctx.exception))

    def test_non_positive_samplerate(self):
        raw = base_project()
        raw["target_sr"] = 0
        with self.assertRaises(ProjectError) as ctx:
            parse_project(raw)
        self.assertIn("target_sr", str(ctx.exception))

    def test_negative_keyframe_time(self):
        raw = base_project()
        raw["tracks"][0]["automation"] = [{"time": -0.1, "db": 0.0}]
        with self.assertRaises(ProjectError) as ctx:
            parse_project(raw)
        self.assertIn("time", str(ctx.exception))

    def test_reject_nan_inf(self):
        raw = base_project()
        raw["tracks"][0]["automation"] = [{"time": 0.0, "db": math.nan}]
        with self.assertRaises(ProjectError):
            parse_project(raw)
        raw["tracks"][0]["automation"] = [{"time": 0.0, "db": math.inf}]
        with self.assertRaises(ProjectError):
            parse_project(raw)

    def test_empty_automation(self):
        raw = base_project()
        raw["tracks"][0]["automation"] = []
        with self.assertRaises(ProjectError) as ctx:
            parse_project(raw)
        self.assertIn("automation", str(ctx.exception))

    def test_hop_must_be_positive(self):
        raw = base_project()
        raw["hop"] = 0
        with self.assertRaises(ProjectError):
            parse_project(raw)

    def test_duplicate_keyframe_later_wins(self):
        raw = base_project()
        raw["tracks"][0]["automation"] = [
            {"time": 1.0, "db": -3.0}, {"time": 1.0, "db": -9.0},
        ]
        project = parse_project(raw)
        self.assertEqual(len(project.tracks[0].automation), 1)
        self.assertEqual(project.tracks[0].automation[0].db, -9.0)

    def test_bus_cycle_rejected_with_code_4(self):
        raw = base_project()
        raw["buses"] = [
            {"name": "a", "output": "b"},
            {"name": "b", "output": "a"},
            {"name": "master", "output": None},
        ]
        raw["tracks"][0]["bus"] = "a"
        project = parse_project(raw)
        from mixctl.errors import GraphError

        with self.assertRaises(GraphError) as ctx:
            build_graph(project)
        self.assertEqual(ctx.exception.exit_code, EXIT_GRAPH)

    def test_self_loop_rejected(self):
        raw = base_project()
        raw["buses"] = [{"name": "master", "output": "master"}]
        raw["tracks"][0]["bus"] = "master"
        project = parse_project(raw)
        from mixctl.errors import GraphError

        with self.assertRaises(GraphError):
            build_graph(project)


class ErrorChainCliTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.fixture_dir = self.tmp / "fixtures"
        generate_fixtures(self.fixture_dir)

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _run(self, *args):
        return subprocess.run(
            [sys.executable, "-m", "mixctl", *args],
            cwd=REPO, capture_output=True, text=True,
        )

    def _copy_project(self) -> Path:
        source = self.fixture_dir / "demo.mix.json"
        target = self.tmp / "proj.mix.json"
        shutil.copy(source, target)
        return target

    def test_missing_file_exit_3(self):
        project_path = self._copy_project()
        project = json.loads(project_path.read_text())
        project["tracks"][0]["file"] = "wav/does_not_exist.wav"
        project_path.write_text(json.dumps(project))
        result = self._run(
            "render", "--project", str(project_path),
            "--out", str(self.tmp / "o.wav"),
        )
        self.assertEqual(result.returncode, EXIT_AUDIO_IO)
        self.assertFalse((self.tmp / "o.wav").exists())

    def test_corrupt_wav_exit_3(self):
        project_path = self._copy_project()
        bad = self.fixture_dir / "wav" / "dialogue.wav"
        bad.write_bytes(b"RIFFxxxxWAVEgarbage-not-a-real-wav")
        result = self._run(
            "render", "--project", str(project_path),
            "--out", str(self.tmp / "o.wav"),
        )
        self.assertEqual(result.returncode, EXIT_AUDIO_IO)

    def test_channel_mismatch_exit_3(self):
        project_path = self._copy_project()
        project = json.loads(project_path.read_text())
        project["tracks"][0]["channels"] = 2
        project_path.write_text(json.dumps(project))
        result = self._run(
            "render", "--project", str(project_path),
            "--out", str(self.tmp / "o.wav"),
        )
        self.assertEqual(result.returncode, EXIT_AUDIO_IO)

    def test_bad_project_exit_2(self):
        project_path = self.tmp / "bad.json"
        project_path.write_text(json.dumps(base_project() | {"target_sr": -1}))
        result = self._run(
            "render", "--project", str(project_path),
            "--out", str(self.tmp / "o.wav"),
        )
        self.assertEqual(result.returncode, EXIT_USAGE)

    def test_missing_project_file_exit_2(self):
        result = self._run(
            "render", "--project", str(self.tmp / "nope.json"),
            "--out", str(self.tmp / "o.wav"),
        )
        self.assertEqual(result.returncode, EXIT_USAGE)

    def test_non_positive_chunk_exit_2(self):
        project_path = self.fixture_dir / "demo.mix.json"
        result = self._run(
            "render", "--project", str(project_path), "--engine", "stream",
            "--chunk", "0", "--out", str(self.tmp / "o.wav"),
        )
        self.assertEqual(result.returncode, EXIT_USAGE)

    def test_bus_cycle_exit_4(self):
        project_path = self._copy_project()
        project = json.loads(project_path.read_text())
        project["buses"] = [
            {"name": "vo", "output": "bed"},
            {"name": "bed", "output": "vo"},
            {"name": "master", "output": None},
        ]
        project_path.write_text(json.dumps(project))
        result = self._run(
            "render", "--project", str(project_path),
            "--out", str(self.tmp / "o.wav"),
        )
        self.assertEqual(result.returncode, EXIT_GRAPH)


if __name__ == "__main__":
    unittest.main()
