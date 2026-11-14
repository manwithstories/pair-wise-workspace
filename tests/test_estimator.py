"""估计器接口与行为用例。"""

import unittest

from abr.estimator import (
    DualModeEstimator,
    EwmaEstimator,
    HarmonicMeanEstimator,
    make_estimator,
)


class TestEstimatorInterface(unittest.TestCase):
    def test_registry_contains_three_impls(self):
        for name in ("harmonic", "ewma", "dual"):
            est = make_estimator(name)
            self.assertEqual(est.estimate(), 0.0)

    def test_unknown_estimator_raises(self):
        with self.assertRaises(KeyError):
            make_estimator("nope")

    def test_harmonic_mean_value(self):
        est = HarmonicMeanEstimator(window=4)
        for x in (1000.0, 2000.0, 4000.0):
            est.add_sample(x)
        expected = 3.0 / (1 / 1000.0 + 1 / 2000.0 + 1 / 4000.0)
        self.assertAlmostEqual(est.estimate(), expected, places=9)

    def test_harmonic_ignores_nonpositive(self):
        est = HarmonicMeanEstimator()
        est.add_sample(0.0)
        est.add_sample(-5.0)
        self.assertEqual(est.estimate(), 0.0)

    def test_ewma_converges_and_dips_on_drop(self):
        est = EwmaEstimator()
        for _ in range(30):
            est.add_sample(5000.0)
        self.assertAlmostEqual(est.estimate(), 5000.0, places=6)
        est.add_sample(500.0)
        self.assertLessEqual(est.estimate(), 5000.0)

    def test_dual_is_robust_to_single_spike(self):
        est = DualModeEstimator(window=12)
        for _ in range(11):
            est.add_sample(2000.0)
        before = est.estimate()
        est.add_sample(20000.0)
        after = est.estimate()
        self.assertLess(after, 4000.0)
        self.assertGreaterEqual(after, before * 0.5)


if __name__ == "__main__":
    unittest.main()
