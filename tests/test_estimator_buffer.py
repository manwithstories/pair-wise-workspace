"""带宽估计器与缓冲下载模拟器测试。"""

from __future__ import annotations

import unittest

from tests._helpers import flat_trace, throughput
from abr.buffer import BufferConfig, BufferSimulator
from abr.estimator import BandwidthEstimator, EstimatorConfig
from abr.trace import Trace
from abr.types import BitrateLevel, NetworkSample, SimulationError, TracePattern


class TestEstimator(unittest.TestCase):
    def test_empty_estimate_is_zero(self):
        est = BandwidthEstimator().estimate()
        self.assertEqual(est.sample_count, 0)
        self.assertEqual(est.estimated_kbps, 0.0)

    def test_safety_discount_applied(self):
        e = BandwidthEstimator(EstimatorConfig(safety_discount=0.8))
        for _ in range(8):
            e.add_sample(throughput(0, 1000.0))
        est = e.estimate()
        self.assertAlmostEqual(est.raw_ema_kbps, 1000.0, places=4)
        self.assertAlmostEqual(est.estimated_kbps, 800.0, places=4)

    def test_outlier_spike_rejected(self):
        e = BandwidthEstimator()
        for _ in range(6):
            e.add_sample(throughput(0, 1000.0))
        self.assertFalse(e.add_sample(throughput(0, 9000.0)))
        self.assertEqual(e.estimate().rejected_outliers, 1)

    def test_sustained_step_eventually_accepted(self):
        e = BandwidthEstimator()
        for _ in range(8):
            e.add_sample(throughput(0, 1000.0))
        accepted = [e.add_sample(throughput(0, 200.0)) for _ in range(8)]
        self.assertTrue(any(accepted))

    def test_reject_non_finite_and_negative(self):
        e = BandwidthEstimator()
        for bad in (float("nan"), float("inf"), -1.0):
            with self.assertRaises(SimulationError):
                e.add_sample(throughput(0, bad))

    def test_window_bounded(self):
        e = BandwidthEstimator(EstimatorConfig(window_size=4))
        for i in range(20):
            e.add_sample(throughput(0, 100.0 + i))
        self.assertEqual(e.estimate().sample_count, 4)

    def test_deterministic(self):
        vals = [500, 800, 1200, 2000, 2100, 1900, 2200, 2400]

        def run():
            e = BandwidthEstimator()
            for i, v in enumerate(vals):
                e.add_sample(throughput(i, float(v)))
            return e.estimate()

        self.assertEqual(run(), run())

    def test_positive_trend_detected(self):
        e = BandwidthEstimator()
        for v in [500, 800, 1200, 1800, 2400, 3000]:
            e.add_sample(throughput(0, float(v)))
        self.assertGreater(e.estimate().trend_kbps_per_sample, 0.0)


class TestBuffer(unittest.TestCase):
    def setUp(self):
        self.level = BitrateLevel(0, 1000.0)  # 2s 段 = 250000 字节
        self.cfg = BufferConfig(segment_duration_s=2.0)

    def test_normal_download_grows_buffer(self):
        sim = BufferSimulator(flat_trace([(0.0, 2000.0)]), self.cfg)
        _, dl, uf, fail = sim.download_segment(0, self.level)
        self.assertIsNone(fail)
        self.assertEqual(uf, 0.0)
        self.assertGreater(sim.buffer_s, self.cfg.target_buffer_s)
        self.assertLess(dl, 2.0)

    def test_throughput_tracks_channel_and_loss(self):
        for loss, expected in ((0.0, 2000.0), (0.3, 1400.0), (0.6, 800.0)):
            sim = BufferSimulator(flat_trace([(0.0, 2000.0)], loss=loss), self.cfg)
            tp, _, _, _ = sim.download_segment(0, self.level)
            self.assertAlmostEqual(tp.throughput_kbps, expected, delta=20.0)

    def test_underflow_and_protection(self):
        low_cfg = BufferConfig(target_buffer_s=1.0, underflow_threshold_s=1.0)
        sim = BufferSimulator(flat_trace([(0.0, 200.0)]), low_cfg)
        sim.buffer_s = 1.0
        _, _, uf, fail = sim.download_segment(0, self.level)
        self.assertIsNone(fail)
        self.assertGreater(uf, 0.0)
        self.assertTrue(sim.in_protection)
        self.assertGreater(sim.total_underflow_s, 0.0)

    def test_protection_releases_after_recovery(self):
        sim = BufferSimulator(
            flat_trace([(0.0, 200.0), (12.0, 8000.0)]),
            BufferConfig(target_buffer_s=10.0, underflow_threshold_s=3.0),
        )
        sim.buffer_s = 1.0
        sim.download_segment(0, self.level)
        self.assertTrue(sim.in_protection)
        for i in range(1, 12):
            sim.download_segment(i, self.level)
        self.assertFalse(sim.in_protection)

    def test_zero_bandwidth_window_fails(self):
        sim = BufferSimulator(
            flat_trace([(0.0, 2000.0), (0.5, 0.0), (40.0, 2000.0)]), self.cfg
        )
        _, _, _, fail = sim.download_segment(0, self.level)
        self.assertIsNotNone(fail)
        self.assertIn("零带宽", fail)

    def test_buffer_capped(self):
        cfg = BufferConfig(target_buffer_s=10.0, max_buffer_s=12.0)
        sim = BufferSimulator(flat_trace([(0.0, 5000.0)]), cfg)
        for i in range(6):
            sim.download_segment(i, self.level)
        self.assertLessEqual(sim.buffer_s, 12.0 + 1e-9)

    def test_trace_exhaustion_diagnosed(self):
        short = Trace(
            [NetworkSample(i * 0.1, 500.0, 40.0, 0.0) for i in range(20)],
            TracePattern.STABLE, 1,
        )
        with self.assertRaises(SimulationError):
            BufferSimulator(short, self.cfg).download_segment(0, self.level)

    def test_deterministic(self):
        def run():
            sim = BufferSimulator(
                flat_trace([(0.0, 1500.0), (5.0, 400.0), (9.0, 1500.0)]), self.cfg
            )
            out = []
            for i in range(6):
                _, dl, uf, _ = sim.download_segment(i, self.level)
                out.append((round(dl, 4), round(uf, 4), round(sim.buffer_s, 4)))
            return out
        self.assertEqual(run(), run())


if __name__ == "__main__":
    unittest.main(verbosity=2)
