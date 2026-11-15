from __future__ import annotations

import copy
import tempfile
import unittest
from pathlib import Path

import numpy as np

from clipgraph.analysis import analyze_array, analyze_wav
from clipgraph.cli import compare_raw
from clipgraph.curve import gain_at_db, sample_curves_db, sample_range
from clipgraph.engine import Engine, RenderState
from clipgraph.fixtures import generate_fixtures
from clipgraph.io import convert_channels, read_info, write_wav
from clipgraph.model import AutomationCurve, CurvePoint, GraphError, graph_from_dict, load_graph
from clipgraph.offline import render_offline
from clipgraph.resample import alignment_info, resample_source
from clipgraph.stream import render_stream


def graph_dict():
    return {
        "project": {"sample_rate": 48000, "channels": 2, "duration": 0.25},
        "nodes": [
            {"id": "d", "kind": "source", "source": {"path": "d", "sample_rate": 32000, "channels": 1, "start_sec": 0.0013}},
            {"id": "m", "kind": "source", "source": {"path": "m", "sample_rate": 44100, "channels": 2, "start_sec": 0.0021}},
            {"id": "dt", "kind": "track", "inputs": ["d"], "curves": [{"id": "dc", "points": [[0.0, 0.0, "linear"], [0.1, -6.0, "cosine"], [0.2, -3.0, "hold"]]}]},
            {"id": "mt", "kind": "track", "inputs": ["m"], "curves": [{"id": "mc", "points": [[0.0, -2.0, "hold"], [0.2, -4.0, "linear"]]}]},
            {"id": "mb", "kind": "bus", "inputs": ["mt"], "sidechain": {"source": "dt", "attack_sec": 0.01, "release_sec": 0.1, "max_duck_db": 8.0, "threshold_db": -30.0}},
            {"id": "master", "kind": "bus", "inputs": ["dt", "mb"], "curves": [{"id": "masterc", "points": [[0.0, -3.0, "linear"], [0.24, -3.0, "hold"]]}]},
        ],
        "outputs": {"master": "master", "tracks": ["dt", "mt"], "buses": ["mb"]},
    }


class GraphTests(unittest.TestCase):
    def test_valid_and_topological_order(self):
        graph = graph_from_dict(graph_dict())
        self.assertLess(graph.topological_order().index("dt"), graph.topological_order().index("mb"))

    def test_duplicate_dangling_cycle_rate_channels(self):
        duplicate = graph_dict()
        duplicate["nodes"].append(copy.deepcopy(duplicate["nodes"][1]))
        with self.assertRaisesRegex(GraphError, "duplicate node id"):
            graph_from_dict(duplicate)
        dangling = graph_dict()
        dangling["nodes"][2]["inputs"] = ["missing"]
        with self.assertRaisesRegex(GraphError, "dangling input"):
            graph_from_dict(dangling)
        cycle = graph_dict()
        cycle["nodes"][4]["inputs"] = ["master"]
        with self.assertRaisesRegex(GraphError, "cycle detected"):
            graph_from_dict(cycle)
        bad_rate = graph_dict()
        bad_rate["project"]["sample_rate"] = 12345
        with self.assertRaisesRegex(GraphError, "sample rate"):
            graph_from_dict(bad_rate)
        bad_channels = graph_dict()
        bad_channels["project"]["channels"] = 0
        with self.assertRaisesRegex(GraphError, "channel count"):
            graph_from_dict(bad_channels)

    def test_curve_time_order(self):
        bad = graph_dict()
        bad["nodes"][2]["curves"][0]["points"] = [[0.0, 0.0, "linear"], [0.2, 1.0, "linear"], [0.1, 2.0, "hold"]]
        with self.assertRaisesRegex(GraphError, "out of order"):
            graph_from_dict(bad)


class CurveTests(unittest.TestCase):
    def test_modes_additive_and_domains(self):
        curve = AutomationCurve("c", (CurvePoint(0.0, 0.0, "hold"), CurvePoint(1.0, 6.0, "linear"), CurvePoint(2.0, 0.0, "hold")))
        values = sample_curves_db((curve,), np.array([0.5, 1.5]))
        np.testing.assert_allclose(values, [0.0, 3.0])
        linear = AutomationCurve("l", (CurvePoint(0.0, 0.5, "hold"), CurvePoint(1.0, 0.5, "hold")), "linear")
        combined = sample_curves_db((linear, curve), np.array([0.5]))
        self.assertAlmostEqual(float(combined[0]), -6.020599913, places=6)
        gains = gain_at_db((curve,), np.array([0.5, 1.5]), -1.0)
        self.assertAlmostEqual(float(gains[1]), 10 ** (2.0 / 20.0), places=9)

    def test_sample_range_boundaries(self):
        curve = AutomationCurve("c", (CurvePoint(0.0, 0.0, "linear"), CurvePoint(1.0, -6.0, "hold")))
        times, values = sample_range((curve,), 0, 48000, 48000, step_frames=12000, block_boundaries=(777, 4097))
        self.assertEqual(len(times), len(values))
        self.assertEqual(len(np.unique(times)), len(times))
        self.assertIn(777 / 48000, times)


class ProcessingTests(unittest.TestCase):
    def test_resample_constant_and_alignment(self):
        source = graph_dict()["nodes"][1]["source"]
        from clipgraph.model import SourceSpec

        spec = SourceSpec(source["path"], 96000, 1, start_sec=0.0011)
        fake = np.full((9600, 1), 0.5, dtype=np.float32)

        def reader(path, start, count):
            out = np.zeros((count, 2), dtype=np.float32)
            part = fake[max(0, start) : start + count, 0]
            out[: len(part)] = part[:, None]
            return out

        result = resample_source(spec, 0, 4800, 48000, 2, reader)
        self.assertTrue(np.all(np.isfinite(result)))
        np.testing.assert_allclose(result[100:1000], 0.5, atol=2e-4)
        info = alignment_info("s", spec, 48000)
        self.assertAlmostEqual(info.fractional_delay_samples, 52.8 % 1.0, places=9)

    def test_engine_variable_blocks_and_nonfinite(self):
        graph = graph_from_dict(graph_dict())
        rng = np.random.default_rng(7)
        d = (rng.standard_normal(7000) * 0.08).astype(np.float32)
        m = (rng.standard_normal((9000, 2)) * 0.04).astype(np.float32)
        d[100:104] = np.nan
        m[200] = np.inf

        def reader(path, start, count):
            src = d if path == "d" else m
            out = np.zeros((count, 2), dtype=np.float32)
            part = src[max(0, start) : start + count]
            out[: len(part)] = part[:, None] if part.ndim == 1 else part
            return out

        engine = Engine(graph, reader)
        whole = engine.render(0, graph.frames, ["master"], RenderState())
        state = RenderState()
        parts = []
        ducks = []
        start = 0
        sizes = (1, 7, 1024, 4097)
        i = 0
        while start < graph.frames:
            end = min(graph.frames, start + sizes[i % len(sizes)])
            result = engine.render(start, end, ["master"], state)
            state = result.state
            parts.append(result.signals["master"])
            ducks.extend(result.duck_db.get("mb", []))
            start = end
            i += 1
        streamed = np.concatenate(parts)
        np.testing.assert_array_equal(whole.signals["master"], streamed)
        self.assertTrue(np.all(np.isfinite(streamed)))
        self.assertGreater(state.nonfinite_zeroed.get("d", 0), 0)
        self.assertGreater(max(ducks), 0.0)

    def test_metrics_clipping_and_true_peak(self):
        signal = np.zeros((1000, 2), dtype=np.float32)
        signal[10:12, 0] = 1.2
        signal[20, 1] = -1.1
        metrics = analyze_array(signal, 48000, window_ms=1.0)
        self.assertEqual(metrics["clip_samples"], 3)
        self.assertEqual(metrics["clip_runs"], 2)
        self.assertGreaterEqual(metrics["true_peak"], 1.2)
        self.assertEqual(len(metrics["rms_windows_dbfs"]), 21)


class EndToEndTests(unittest.TestCase):
    def test_channel_conversion_and_bit_depth(self):
        mono = np.ones((8, 1), dtype=np.float32)
        stereo = convert_channels(mono, 2)
        self.assertEqual(stereo.shape, (8, 2))
        np.testing.assert_array_equal(convert_channels(stereo, 1), mono)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "tone.wav"
            write_wav(path, stereo * 0.1, 48000, 24)
            info = read_info(path)
            self.assertEqual(info.subtype, "PCM_24")
            self.assertEqual((info.sample_rate, info.channels), (48000, 2))

    def test_fixture_offline_stream_compare_analyze(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            generated = generate_fixtures(root / "fixtures", 7)
            graph = load_graph(generated["graph"])
            render = render_offline(graph, root / "fixtures", root / "a.wav", root / "a.f32", root / "render.json", block_size=257)
            stream = render_stream(graph, root / "fixtures", [1, 7, 1024, 4097], root / "b.f32", root / "stream.json")
            compared = compare_raw(str(root / "a.f32"), str(root / "b.f32"), 1e-9)
            self.assertEqual(compared["max_abs_diff"], 0.0)
            self.assertEqual(compared["first_mismatch"], -1)
            self.assertGreater(render["nonfinite"]["total"], 0)
            self.assertEqual(render["tracks"]["empty_cue"]["frames"], graph.frames)
            self.assertGreaterEqual(stream["buses"]["music_bus"]["duck_max_db"], -9.0)
            analyzed = analyze_wav(str(root / "a.wav"), 400.0)
            self.assertTrue(np.isfinite(analyzed["peak_dbfs"]))


if __name__ == "__main__":
    unittest.main()
