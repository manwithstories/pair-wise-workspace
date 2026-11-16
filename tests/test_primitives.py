"""数值原语层测试:重采样、自动化、混音图、指标。

覆盖:确定性、边界保持、声道映射禁令、削波阈值与 dBFS 兜底。
"""

import unittest

import numpy as np

from mixer.automation import AutomationCurve, total_automation_samples
from mixer.graph import (
    apply_gain,
    count_clipped,
    map_to_stereo,
    place_track,
    render_track_block,
)
from mixer.metrics import MetricsAccumulator, TrackMetrics, to_dbfs
from mixer.resample import Resampler, resample_whole
from mixer.types import AutomationPoint, ConfigError, TrackSpec


class ResamplerTest(unittest.TestCase):
    def test_output_length_is_ceil_ratio(self):
        for in_rate, out_rate, n in [
            (44100, 48000, 1000),
            (48000, 44100, 1000),
            (96000, 48000, 1001),
            (48000, 48000, 777),
        ]:
            y = resample_whole(np.arange(n, dtype=np.float64), in_rate, out_rate)
            self.assertEqual(y.shape[0], int(np.ceil(n * out_rate / in_rate)))

    def test_chunking_is_bit_exact(self):
        rng = np.random.default_rng(7)
        x = rng.standard_normal(4000)
        for in_rate, out_rate in [(44100, 48000), (48000, 44100), (96000, 48000)]:
            whole = resample_whole(x, in_rate, out_rate)
            for bs in [1, 3, 257, 1024, 4000]:
                rs = Resampler(in_rate, out_rate)
                parts = [rs.feed(x[i:i + bs]) for i in range(0, len(x), bs)]
                parts.append(rs.flush())
                chunked = np.concatenate(parts)
                np.testing.assert_array_equal(chunked, whole)

    def test_identity_passthrough(self):
        x = np.linspace(-1, 1, 500)
        rs = Resampler(48000, 48000)
        np.testing.assert_array_equal(rs.feed(x), x)
        self.assertEqual(rs.flush().shape[0], 0)

    def test_dc_gain_preserved(self):
        dc = np.ones(4000)
        for in_rate, out_rate in [(44100, 48000), (48000, 44100), (96000, 48000)]:
            y = resample_whole(dc, in_rate, out_rate)
            self.assertLess(np.max(np.abs(y[40:-40] - 1.0)), 1e-9)

    def test_downsample_suppresses_alias(self):
        t = np.arange(9600)
        hf = np.sin(2 * np.pi * 0.45 * t)  # 43.2kHz,高于 48k Nyquist
        y = resample_whole(hf, 96000, 48000)
        self.assertLess(np.sqrt(np.mean(y[100:-100] ** 2)), 0.2)

    def test_multichannel_shape(self):
        rng = np.random.default_rng(11)
        xs = rng.standard_normal((1500, 3))
        y = resample_whole(xs, 44100, 48000)
        self.assertEqual(y.ndim, 2)
        self.assertEqual(y.shape[1], 3)

    def test_rejects_non_positive_rate(self):
        with self.assertRaises(ConfigError):
            Resampler(0, 48000)
        with self.assertRaises(ConfigError):
            Resampler(48000, -1)

    def test_tiny_input_is_finite(self):
        y = resample_whole(np.ones(2), 44100, 48000)
        self.assertTrue(np.isfinite(y).all())
        self.assertEqual(y.shape[0], 3)


class AutomationTest(unittest.TestCase):
    def test_sorts_unsorted_and_interpolates(self):
        curve = AutomationCurve([
            AutomationPoint(10, 2.0),
            AutomationPoint(0, 0.0),
            AutomationPoint(5, 1.0),
        ])
        y = curve.sample(np.arange(12))
        self.assertEqual(y[0], 0.0)
        self.assertEqual(y[5], 1.0)
        self.assertEqual(y[10], 2.0)
        self.assertAlmostEqual(y[7], 1.4)

    def test_holds_first_and_last_value(self):
        curve = AutomationCurve([AutomationPoint(4, 1.0), AutomationPoint(8, 3.0)])
        y = curve.sample(np.array([0, 4, 8, 9, 50]))
        self.assertEqual(y.tolist(), [1.0, 1.0, 3.0, 3.0, 3.0])

    def test_empty_curve_is_unit_gain(self):
        y = AutomationCurve([]).sample(np.arange(5))
        np.testing.assert_array_equal(y, np.ones(5))

    def test_single_point_is_constant(self):
        y = AutomationCurve([AutomationPoint(2, 0.7)]).sample(np.arange(6))
        np.testing.assert_array_equal(y, np.full(6, 0.7))

    def test_rejects_invalid_points(self):
        cases = [
            [AutomationPoint(1, 1.0), AutomationPoint(1, 2.0)],      # 重复时间
            [AutomationPoint(0, float("nan"))],
            [AutomationPoint(0, float("inf"))],
            [AutomationPoint(-1, 1.0)],
        ]
        for pts in cases:
            with self.assertRaises(ConfigError):
                AutomationCurve(pts)

    def test_rejects_negative_sample_position(self):
        curve = AutomationCurve([AutomationPoint(0, 1.0)])
        with self.assertRaises(ConfigError):
            curve.sample(np.array([-1]))

    def test_counts_points(self):
        tracks = [
            TrackSpec(name="a", path="a", automation=(AutomationPoint(0, 1),)),
            TrackSpec(name="b", path="b", automation=(
                AutomationPoint(0, 1), AutomationPoint(1, 2))),
        ]
        self.assertEqual(total_automation_samples(tracks), 3)


class GraphTest(unittest.TestCase):
    def test_mono_duplicated_to_stereo(self):
        mono = np.array([[1.0], [2.0]])
        out = map_to_stereo(mono, (0, 1))
        np.testing.assert_array_equal(out, np.repeat(mono, 2, axis=1))
        out2 = map_to_stereo(mono[:, 0], (0, 0))
        np.testing.assert_array_equal(out2, out)

    def test_stereo_swap(self):
        stereo = np.array([[1.0, 2.0], [3.0, 4.0]])
        np.testing.assert_array_equal(
            map_to_stereo(stereo, (1, 0)), stereo[:, ::-1]
        )

    def test_multichannel_requires_full_route(self):
        three = np.ones((2, 3))
        with self.assertRaises(ConfigError):
            map_to_stereo(three, (0, 1))
        out = map_to_stereo(three, (0, 1), routes={0: "L", 1: "R", 2: "L+R"})
        np.testing.assert_array_equal(out, np.ones((2, 2)) * 2.0)
        with self.assertRaises(ConfigError):
            map_to_stereo(three, (0, 1), routes={0: "L", 1: "R"})

    def test_index_out_of_range(self):
        stereo = np.zeros((2, 2))
        with self.assertRaises(ConfigError):
            map_to_stereo(stereo, (0, 2))

    def test_apply_gain(self):
        out = apply_gain(np.ones((4, 2)), 0.5, np.array([1, 2, 1, 2], float))
        np.testing.assert_allclose(out[:, 0], [0.5, 1.0, 0.5, 1.0])

    def test_place_track_offsets_and_clips(self):
        block = np.zeros((5, 2))
        place_track(block, np.ones((3, 2)), 2)
        self.assertEqual(block[:, 0].tolist(), [0, 0, 1, 1, 1])
        block = np.zeros((5, 2))
        place_track(block, np.ones((10, 2)), 3)
        self.assertEqual(block[:, 0].tolist(), [0, 0, 0, 1, 1])
        block = np.zeros((5, 2))
        place_track(block, np.ones((2, 2)), -1)
        self.assertEqual(block[:, 0].tolist(), [1, 0, 0, 0, 0])

    def test_clip_count_and_render(self):
        sig = np.array([[0.9, -1.0], [1.0, 0.2], [0.1, 0.1]])
        self.assertEqual(count_clipped(sig, 1.0), 2)
        self.assertEqual(count_clipped(np.zeros((0, 2)), 1.0), 0)
        spec = TrackSpec(name="x", path="x", gain=2.0)
        out = render_track_block(spec, np.ones((3, 1)), np.ones(3))
        np.testing.assert_array_equal(out, np.full((3, 2), 2.0))


class MetricsTest(unittest.TestCase):
    def test_rms_and_effective_rms(self):
        sig = np.array([[0.5, -0.5], [0.5, 0.5], [0.0, 0.0], [0.0, 0.0]])
        acc = MetricsAccumulator()
        for row in sig:
            acc.add(row[None, :])
        snap = acc.snapshot()
        self.assertAlmostEqual(snap["rms"], np.sqrt(1.0 / 2 / 4))
        self.assertAlmostEqual(snap["effective_rms"], np.sqrt(1.0 / 2 / 2))
        self.assertEqual(snap["peak"], 0.5)
        self.assertEqual(snap["active_frames"], 2)

    def test_silence_uses_floor(self):
        acc = MetricsAccumulator()
        acc.add(np.zeros((3, 2)))
        self.assertEqual(acc.snapshot()["rms_dbfs"], -120.0)
        self.assertEqual(to_dbfs(0.0), -120.0)

    def test_configurable_threshold(self):
        acc = MetricsAccumulator(clip_threshold=0.9)
        acc.add(np.array([[0.95, 0.0], [1.2, -1.2], [0.1, 0.1]]))
        self.assertEqual(acc.clipped_samples, 2)

    def test_track_metrics(self):
        tm = TrackMetrics(1.0)
        tm.add(np.array([[1.0, 0.5]]))
        tm.add(np.array([[0.2, -1.1]]))
        self.assertEqual(tm.peak, 1.1)
        self.assertEqual(tm.clipped_samples, 2)


if __name__ == "__main__":
    unittest.main()
