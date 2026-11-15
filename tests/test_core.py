from __future__ import annotations

import csv
import tempfile
import unittest
import warnings
from pathlib import Path

import numpy as np
import soundfile as sf

from mixer.automation import AutomationCurve, AutomationError
from mixer.engine import render_to_file
from mixer.graph import GraphError, parse_graph, topological_sort
from mixer.io import read_audio
from mixer.metrics import MetricsAccumulator, compute_metrics
from mixer.resample import (
    RationalResampler,
    rational_ratio,
    resample_array,
    resampled_length,
)
from mixer.stream import stream_to_file


class GraphTests(unittest.TestCase):
    def test_valid_topological_order(self) -> None:
        graph = parse_graph(
            {
                "output": "out",
                "nodes": [
                    {"id": "a", "type": "source", "path": "a.wav"},
                    {"id": "b", "type": "source", "path": "b.wav"},
                    {"id": "sum", "type": "sum", "inputs": ["a", "b"]},
                    {"id": "out", "type": "limiter", "input": "sum"},
                ],
            }
        )
        order = topological_sort(graph)
        self.assertEqual(graph.output, "out")
        self.assertLess(order.index("a"), order.index("sum"))
        self.assertLess(order.index("out"), len(order))

    def test_validation_errors(self) -> None:
        cases = {
            "unknown": {"nodes": [{"id": "a", "type": "verb"}]},
            "duplicate": {
                "nodes": [
                    {"id": "a", "type": "source", "path": "a.wav"},
                    {"id": "a", "type": "source", "path": "b.wav"},
                ]
            },
            "dangling": {"nodes": [{"id": "g", "type": "gain", "input": "x"}]},
            "cycle": {
                "nodes": [
                    {"id": "a", "type": "gain", "input": "b"},
                    {"id": "b", "type": "gain", "input": "a"},
                ]
            },
            "outputs": {
                "nodes": [
                    {"id": "a", "type": "source", "path": "a.wav"},
                    {"id": "b", "type": "source", "path": "b.wav"},
                ]
            },
        }
        for name, data in cases.items():
            with self.subTest(name=name), self.assertRaises(GraphError):
                parse_graph(data)


class AutomationTests(unittest.TestCase):
    def test_endpoint_hold_and_interpolation(self) -> None:
        curve = AutomationCurve.from_points([(1.0, -6.0), (2.0, 0.0), (4.0, 3.0)])
        self.assertEqual(curve.evaluate(0.0), -6.0)
        self.assertEqual(curve.evaluate(1.5), -3.0)
        self.assertEqual(curve.evaluate(9.0), 3.0)
        self.assertEqual(float(AutomationCurve.empty().evaluate(12.0)), 0.0)

    def test_seek_timing(self) -> None:
        curve = AutomationCurve.from_points(
            [(0.0, 0.0), (0.123456, 7.5), (1.0, -2.0)]
        )
        first = float(curve.evaluate(2400 / 48000.0))
        expected = 7.5 * (2400 / 48000.0) / 0.123456
        self.assertLess(abs(first - expected), 1e-9)

    def test_invalid_csv(self) -> None:
        rows = [
            [(0.0, 0.0), (0.0, 1.0)],
            [(-0.1, 0.0)],
            [(0.0, float("nan"))],
            [(0.0, float("inf"))],
        ]
        for points in rows:
            with tempfile.NamedTemporaryFile("w", suffix=".csv") as handle:
                writer = csv.writer(handle)
                writer.writerow(["time_sec", "gain_db"])
                writer.writerows(points)
                handle.flush()
                with self.subTest(points=points), self.assertRaises(AutomationError):
                    AutomationCurve.from_csv(handle.name)


class ResampleTests(unittest.TestCase):
    def test_ratios_and_lengths(self) -> None:
        self.assertEqual(str(rational_ratio(44100, 48000)), "160/147")
        self.assertEqual(str(rational_ratio(32000, 48000)), "3/2")
        self.assertEqual(resampled_length(44100, 160, 147), 48000)
        self.assertEqual(resampled_length(1, 1, 2), 1)

    def test_block_boundary_invariance(self) -> None:
        rng = np.random.default_rng(42)
        data = rng.normal(size=(777, 2))
        for source_sr, target_sr in ((44100, 48000), (48000, 32000), (32000, 48000)):
            whole = resample_array(data, source_sr, target_sr)
            converter = RationalResampler(source_sr, target_sr, channels=2)
            pieces = []
            position = 0
            while position < data.shape[0]:
                count = int(rng.integers(1, 70))
                pieces.append(converter.process(data[position : position + count]))
                position += count
            pieces.append(converter.flush())
            chunked = np.concatenate(pieces)
            self.assertEqual(whole.shape, chunked.shape)
            np.testing.assert_array_equal(whole, chunked)


class MetricsTests(unittest.TestCase):
    def test_silence_uses_null_rms(self) -> None:
        metrics = compute_metrics(np.zeros((100, 2)), 48000)
        self.assertIsNone(metrics.rms_dbfs)
        self.assertIsNone(metrics.peak_dbfs)
        self.assertEqual(metrics.clip_count, 0)
        self.assertEqual(metrics.clip_runs, 0)

    def test_clip_runs_cross_blocks(self) -> None:
        accumulator = MetricsAccumulator(100)
        accumulator.update(np.array([[1.1, 0.0], [0.0, 0.0]]), start_frame=0)
        accumulator.update(np.array([[1.1, 0.0], [1.0, 0.0]]), start_frame=2)
        snapshot = accumulator.snapshot()
        self.assertEqual(snapshot.clip_count, 3)
        self.assertEqual(snapshot.clip_runs, 2)
        self.assertEqual(snapshot.clip_times[0], 0.0)


class RenderPathTests(unittest.TestCase):
    def test_offline_and_stream_match(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source_sr = 44100
            seconds = 0.35
            t = np.arange(int(source_sr * seconds)) / source_sr
            source = 0.2 * np.sin(2 * np.pi * 440 * t)
            sf.write(root / "source.wav", source, source_sr, subtype="PCM_24")
            graph = parse_graph(
                {
                    "master_sr": 48000,
                    "output": "out",
                    "nodes": [
                        {"id": "src", "type": "source", "path": "source.wav"},
                        {
                            "id": "gain",
                            "type": "gain",
                            "input": "src",
                            "gain_db": -3,
                        },
                        {"id": "pan", "type": "pan", "input": "gain", "pan": 0.2},
                        {"id": "sum", "type": "sum", "inputs": ["pan"]},
                        {"id": "out", "type": "limiter", "input": "sum"},
                    ],
                }
            )
            offline = render_to_file(
                graph, root / "offline.wav", 48000, 128, workspace=root
            )
            streamed = stream_to_file(
                graph, root / "stream.wav", 48000, 37, workspace=root
            )
            self.assertEqual(offline.frames_written, streamed.frames_written)
            a, _ = read_audio(root / "offline.wav")
            b, _ = read_audio(root / "stream.wav")
            np.testing.assert_array_equal(a, b)

    def test_multichannel_source_warns(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "three.wav"
            sf.write(path, np.zeros((100, 3)), 48000, subtype="PCM_16")
            with warnings.catch_warnings(record=True) as caught:
                warnings.simplefilter("always")
                data, _ = read_audio(path)
            self.assertEqual(data.shape[1], 2)
            self.assertEqual(len(caught), 1)


if __name__ == "__main__":
    unittest.main()
