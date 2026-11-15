import unittest

from support import ROOT  # noqa: F401

import numpy as np

from abrl.ladder.builder import (
    REDUNDANCY_DB,
    build_ladder,
    evaluate,
)
from abrl.media.yuv import Frame
from abrl.metrics.quality import PSNR_IDENTICAL, aggregate, psnr, ssim


def _frame(plane: np.ndarray) -> Frame:
    half = plane[::2, ::2]
    return Frame(plane, half, half)


class QualityTests(unittest.TestCase):
    def test_identical_frame_sentinel(self):
        plane = np.full((32, 32), 90, np.uint8)
        self.assertEqual(psnr(plane, plane), PSNR_IDENTICAL)
        self.assertEqual(ssim(plane, plane), 1.0)

    def test_ssim_bounded_and_four_decimals(self):
        rng = np.random.default_rng(3)
        ref = rng.integers(0, 256, (40, 40), dtype=np.uint8)
        noisy = np.clip(ref.astype(int) + rng.normal(0, 12, ref.shape), 0, 255).astype(np.uint8)
        value = ssim(ref, noisy)
        self.assertLess(0.0, value < 1.0)
        self.assertEqual(round(value, 4), value)

    def test_psnr_degrades_with_noise(self):
        rng = np.random.default_rng(4)
        ref = rng.integers(0, 256, (48, 48), dtype=np.uint8)
        mild = np.clip(ref + rng.normal(0, 3, ref.shape), 0, 255).astype(np.uint8)
        heavy = np.clip(ref + rng.normal(0, 25, ref.shape), 0, 255).astype(np.uint8)
        self.assertGreater(psnr(ref, mild), psnr(ref, heavy))

    def test_aggregate_reports_per_frame_arrays(self):
        rng = np.random.default_rng(5)
        refs = [_frame(rng.integers(0, 256, (32, 32), dtype=np.uint8)) for _ in range(3)]
        dist = [
            _frame(np.clip(f.y.astype(int) + 5, 0, 255).astype(np.uint8))
            for f in refs
        ]
        result = aggregate(refs, dist)
        self.assertEqual(result["frames"], 3)
        self.assertEqual(len(result["per_frame_psnr"]), 3)
        self.assertIn("ssim", result)


def _candidate(key: str, kbps: float, quality: float, width=64, height=32):
    return {
        "key": key, "width": width, "height": height, "fps": 30, "qp": 20,
        "actual_kbps": kbps, "psnr_db": quality, "ssim": 0.95,
        "bytes": 1000, "duration_s": 1.0,
    }


class LadderTests(unittest.TestCase):
    def test_strict_monotone_ladder(self):
        candidates = [
            _candidate("a", 100, 28.0),
            _candidate("b", 180, 31.0),
            _candidate("c", 320, 34.5),
            _candidate("d", 600, 38.0),
        ]
        result = build_ladder(candidates, rungs=4, max_kbps=1000,
                              min_ratio=1.3, max_ratio=2.2)
        self.assertTrue(result.feasible)
        self.assertEqual([r["key"] for r in result.rungs], ["a", "b", "c", "d"])
        checks = evaluate(result.rungs, 1.3, 2.2, 1000)
        self.assertTrue(all(checks.values()))

    def test_redundant_rungs_are_merged(self):
        candidates = [
            _candidate("a", 100, 28.0),
            _candidate("b", 180, 28.2),
            _candidate("c", 320, 34.0),
            _candidate("d", 600, 39.0),
        ]
        result = build_ladder(candidates, rungs=3, max_kbps=1000,
                              min_ratio=1.1, max_ratio=4.0)
        self.assertTrue(result.feasible, result.reason)
        self.assertEqual(len(result.rungs), 3)
        keys = [rung["key"] for rung in result.rungs]
        self.assertNotIn("a", keys)
        self.assertIn("b", keys)
        for low, high in zip(result.rungs, result.rungs[1:]):
            self.assertGreaterEqual(high["psnr_db"] - low["psnr_db"], REDUNDANCY_DB - 1e-9)

    def test_infeasible_diagnosis_and_closest_count(self):
        candidates = [
            _candidate("a", 100, 28.0),
            _candidate("b", 110, 31.0),
            _candidate("c", 120, 34.0),
        ]
        result = build_ladder(candidates, rungs=3, max_kbps=1000,
                              min_ratio=1.5, max_ratio=2.2)
        self.assertFalse(result.feasible)
        self.assertIn("closest feasible rung count", result.reason)
        self.assertGreaterEqual(result.closest_feasible_count, 1)

    def test_cap_rejects_too_high_rung(self):
        candidates = [_candidate("a", 100, 28.0), _candidate("b", 300, 34.0)]
        result = build_ladder(candidates, rungs=2, max_kbps=200,
                              min_ratio=1.2, max_ratio=3.0)
        self.assertFalse(result.feasible)
        self.assertEqual(result.closest_feasible_count, 1)


if __name__ == "__main__":
    unittest.main()
