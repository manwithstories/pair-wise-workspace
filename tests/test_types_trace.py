"""数据结构校验与网络轨迹测试。"""

from __future__ import annotations

import unittest

from tests._helpers import BITRATES, flat_trace
from abr.trace import Trace, TraceConfig, TraceGenerator
from abr.types import (
    BitrateLevel,
    ConfigError,
    NetworkSample,
    TraceError,
    TracePattern,
    validate_bitrates,
)


class TestTypesAndValidation(unittest.TestCase):
    def test_strictly_increasing_bitrates(self):
        levels = validate_bitrates(list(BITRATES))
        self.assertEqual([l.bitrate_kbps for l in levels], list(BITRATES))
        self.assertEqual(len(levels), 6)

    def test_reject_non_increasing(self):
        for bad in ([100, 100], [200, 100], [100, 200, 200]):
            with self.assertRaises(ConfigError):
                validate_bitrates(bad)

    def test_reject_non_finite_and_negative(self):
        for bad in ([100, float("nan")], [100, float("inf")],
                    [100, -5.0], [0.0, 100.0]):
            with self.assertRaises(ConfigError):
                validate_bitrates(bad)

    def test_too_few_levels(self):
        with self.assertRaises(ConfigError):
            validate_bitrates([100.0])

    def test_segment_bytes(self):
        # 1000 kbit/s * 2 s / 8 = 250000 bytes
        self.assertEqual(BitrateLevel(0, 1000.0).segment_bytes(2.0), 250_000)

    def test_immutability(self):
        lvl = validate_bitrates([100.0, 200.0])[0]
        with self.assertRaises(Exception):
            lvl.bitrate_kbps = 999.0  # type: ignore[misc]


class TestTrace(unittest.TestCase):
    def test_deterministic_same_seed(self):
        cfg = TraceConfig(duration_s=60, seed=42, pattern=TracePattern.MIXED)
        a = [s.bandwidth_kbps for s in TraceGenerator(cfg).generate().samples]
        b = [s.bandwidth_kbps for s in TraceGenerator(cfg).generate().samples]
        self.assertEqual(a, b)

    def test_different_seed_differs(self):
        c1 = TraceConfig(duration_s=60, seed=1)
        c2 = TraceConfig(duration_s=60, seed=2)
        a = [s.bandwidth_kbps for s in TraceGenerator(c1).generate().samples]
        b = [s.bandwidth_kbps for s in TraceGenerator(c2).generate().samples]
        self.assertNotEqual(a, b)

    def test_all_patterns_generate_valid_trace(self):
        for pattern in TracePattern:
            trace = TraceGenerator(
                TraceConfig(duration_s=30, seed=3, pattern=pattern)
            ).generate()
            self.assertGreater(len(trace.samples), 0)
            self.assertGreaterEqual(
                max(s.bandwidth_kbps for s in trace.samples), 0.0
            )

    def test_burst_pattern_contains_zero_bandwidth(self):
        trace = TraceGenerator(
            TraceConfig(duration_s=60, seed=1, pattern=TracePattern.BURST_LOSS)
        ).generate()
        self.assertTrue(any(s.bandwidth_kbps == 0.0 for s in trace.samples))

    def test_interpolation(self):
        # 阶跃从 t=0.5 开始：t=0.45 恰在 0.4(1000) 与 0.5(2000) 中点
        trace = flat_trace([(0.0, 1000.0), (0.5, 2000.0)], duration=2.0)
        self.assertAlmostEqual(
            trace.sample_at(0.45).bandwidth_kbps, 1500.0, delta=1.0
        )

    def test_exhaustion_raises(self):
        with self.assertRaises(TraceError):
            flat_trace([(0.0, 1000.0)], duration=1.0).sample_at(5.0)

    def test_reject_all_zero_trace(self):
        with self.assertRaises(TraceError):
            Trace([NetworkSample(0.0, 0.0, 10.0, 0.0),
                   NetworkSample(0.1, 0.0, 10.0, 0.0)],
                  TracePattern.STABLE, 1)

    def test_reject_nan_and_bad_loss(self):
        with self.assertRaises(TraceError):
            Trace([NetworkSample(0.0, float("nan"), 10.0, 0.0)],
                  TracePattern.STABLE, 1)
        with self.assertRaises(TraceError):
            Trace([NetworkSample(0.0, 100.0, 10.0, 1.0)],
                  TracePattern.STABLE, 1)

    def test_invalid_config(self):
        with self.assertRaises(TraceError):
            TraceConfig(duration_s=0).validate()


if __name__ == "__main__":
    unittest.main(verbosity=2)
