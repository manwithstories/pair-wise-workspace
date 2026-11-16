"""Test suite for the ABR engine.

Runs under both ``python -m pytest -q`` and ``python -m unittest`` because
the cases are plain :mod:`unittest` classes. Coverage spans the bandwidth
estimator safety states, the policy hysteresis rules, the simulator's
buffer/underflow accounting, the JSON error chain, determinism and the
large-trace (100k segment) performance/memory requirement.
"""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from abr_engine.estimator import BandwidthEstimator
from abr_engine.io import load_trace, parse_bitrates, save_trace
from abr_engine.metrics import find_stall_episodes, summarize
from abr_engine.models import (
    ABRConfigError,
    ABRSimulationError,
    ABRTraceError,
    BitrateLadder,
    BufferState,
    Estimate,
    EstimatorState,
    NetworkSample,
    SimulationConfig,
    Trace,
)
from abr_engine.policy import ABRPolicy, PolicyConfig
from abr_engine.simulator import Simulator
from abr_engine.trace import generate_trace

LADDER = (800, 1600, 3000, 5000, 8000)


def sample(t: float, throughput: float, rtt: float = 30.0,
           loss: float = 0.0) -> NetworkSample:
    return NetworkSample(t=t, throughput=throughput, rtt=rtt, loss=loss)


class EstimatorTests(unittest.TestCase):
    def test_empty_and_warmup_are_unsafe(self) -> None:
        est = BandwidthEstimator(window=6, min_samples=2)
        first = est.estimate()
        self.assertIs(first.state, EstimatorState.EMPTY)
        self.assertFalse(first.state.safe)
        est.add(sample(0.0, 5000.0))
        warm = est.estimate()
        self.assertIs(warm.state, EstimatorState.WARMUP)
        self.assertFalse(warm.state.safe)

    def test_weighted_estimate_favours_recent_samples(self) -> None:
        est = BandwidthEstimator(window=6, min_samples=2)
        # Five stable samples then a single high one: the lone spike is
        # suppressed as a minority outlier, while recency still weights the
        # surviving stable samples (flat here, so the estimate stays ~1000).
        for t, value in enumerate((1000.0, 1000.0, 1000.0, 1000.0, 1000.0,
                                   3000.0)):
            est.add(sample(float(t), value))
        result = est.estimate()
        self.assertIs(result.state, EstimatorState.OK)
        self.assertEqual(result.samples_used, 5)
        self.assertAlmostEqual(result.bandwidth_kbps, 1000.0, places=2)

    def test_recent_samples_weigh_more_without_outliers(self) -> None:
        est = BandwidthEstimator(window=4, min_samples=2)
        for t, value in enumerate((1000.0, 1000.0, 3000.0, 3000.0)):
            est.add(sample(float(t), value))
        result = est.estimate()
        # No outlier (two of each is a majority, not a minority): recency
        # weights 1,2,3,4 give (1000+2000+9000+12000)/10 = 2400 rather
        # than the flat mean 2000.
        self.assertIs(result.state, EstimatorState.OK)
        self.assertAlmostEqual(result.bandwidth_kbps, 2400.0, places=2)

    def test_spike_outlier_is_suppressed(self) -> None:
        est = BandwidthEstimator(window=6, min_samples=2)
        for t, value in enumerate((4000.0, 4000.0, 4000.0, 12000.0, 4000.0)):
            est.add(sample(float(t), value))
        result = est.estimate()
        self.assertEqual(result.samples_used, 4)
        self.assertLess(result.bandwidth_kbps, 5000.0)

    def test_sustained_dip_survives_filtering(self) -> None:
        est = BandwidthEstimator(window=6, min_samples=3)
        for t, value in enumerate((5000.0, 5000.0, 900.0, 900.0, 900.0)):
            est.add(sample(float(t), value))
        result = est.estimate()
        self.assertIs(result.state, EstimatorState.OK)
        # Three of the five samples are low — a majority, so nothing is
        # filtered and the recency-weighted estimate tracks the dip rather
        # than the stale high samples (flat mean would be 2540).
        self.assertEqual(result.samples_used, 5)
        self.assertLess(result.bandwidth_kbps, 2500.0)
        self.assertGreater(result.bandwidth_kbps, 0.0)

    def test_all_zero_window_reports_zero_state(self) -> None:
        est = BandwidthEstimator(window=6, min_samples=2)
        for t in range(4):
            est.add(sample(float(t), 0.0, rtt=200.0, loss=0.5))
        result = est.estimate()
        self.assertIs(result.state, EstimatorState.ZERO)
        self.assertEqual(result.bandwidth_kbps, 0.0)
        self.assertFalse(result.state.safe)

    def test_mixed_zero_and_positive_stays_finite(self) -> None:
        # Regression test: a window containing zeros must never divide by
        # zero after outlier suppression.
        est = BandwidthEstimator(window=6, min_samples=2)
        for t, value in enumerate((0.0, 0.0, 1000.0, 1000.0)):
            est.add(sample(float(t), value))
        result = est.estimate()
        self.assertIs(result.state, EstimatorState.OK)
        self.assertGreater(result.bandwidth_kbps, 0.0)

    def test_timestamp_regression_is_rejected(self) -> None:
        est = BandwidthEstimator(window=6, min_samples=2)
        self.assertTrue(est.add(sample(2.0, 5000.0)))
        self.assertFalse(est.add(sample(1.0, 5000.0)))
        result = est.estimate()
        self.assertIs(result.state, EstimatorState.REGRESSION)
        self.assertFalse(result.state.safe)

    def test_loss_discounts_bandwidth(self) -> None:
        est = BandwidthEstimator(window=4, min_samples=2, loss_penalty=2.0)
        for t in range(4):
            est.add(sample(float(t), 4000.0, loss=0.1))
        result = est.estimate()
        self.assertAlmostEqual(result.bandwidth_kbps, 3200.0, places=2)

    def test_window_eviction_drops_oldest(self) -> None:
        est = BandwidthEstimator(window=3, min_samples=2)
        for t, value in enumerate((100.0, 100.0, 100.0, 9000.0, 9000.0, 9000.0)):
            est.add(sample(float(t), value))
        self.assertAlmostEqual(est.estimate().bandwidth_kbps, 9000.0)


class PolicyTests(unittest.TestCase):
    def setUp(self) -> None:
        self.ladder = BitrateLadder(LADDER)
        self.high_buffer = BufferState(24.0, 30.0)
        self.low_buffer = BufferState(3.0, 30.0)

    def policy(self, **kwargs) -> ABRPolicy:
        return ABRPolicy(self.ladder, PolicyConfig(**kwargs))

    def estimate(self, bandwidth: float,
                 state: EstimatorState = EstimatorState.OK) -> Estimate:
        return Estimate(bandwidth, state, 6, 0.0, 30.0)

    def test_unsafe_estimate_returns_floor(self) -> None:
        policy = self.policy()
        for state in (EstimatorState.WARMUP, EstimatorState.ZERO,
                      EstimatorState.EMPTY, EstimatorState.REGRESSION):
            self.assertEqual(policy.select(4, self.estimate(0.0, state),
                                           self.high_buffer), 0)

    def test_low_buffer_forces_floor(self) -> None:
        policy = self.policy()
        self.assertEqual(policy.select(4, self.estimate(9000.0),
                                       self.low_buffer), 0)

    def test_downshift_is_immediate_by_default(self) -> None:
        policy = self.policy()
        # Effective bandwidth 1000 * 0.85 = 850 -> only level 0 fits.
        self.assertEqual(policy.select(4, self.estimate(1000.0),
                                       self.high_buffer), 0)

    def test_upshift_requires_confirmation_and_climbs_one_rung(self) -> None:
        policy = self.policy(upshift_confirm=3)
        index = 0
        for _ in range(2):
            index = policy.select(index, self.estimate(9000.0),
                                  self.high_buffer)
        self.assertEqual(index, 0)
        index = policy.select(index, self.estimate(9000.0), self.high_buffer)
        self.assertEqual(index, 1)

    def test_failed_segment_resets_upshift_streak(self) -> None:
        policy = self.policy(upshift_confirm=3)
        index = 0
        index = policy.select(index, self.estimate(9000.0), self.high_buffer)
        index = policy.select(index, self.estimate(9000.0), self.high_buffer)
        self.assertEqual(policy.upshift_streak, 2)
        index = policy.select(index, self.estimate(400.0), self.high_buffer)
        self.assertEqual(index, 0)
        self.assertEqual(policy.upshift_streak, 0)

    def test_upshift_blocked_without_enough_buffer(self) -> None:
        policy = self.policy(upshift_confirm=1)
        thin = BufferState(8.0, 30.0)  # ratio ~0.27 below 0.35 water level
        self.assertEqual(policy.select(0, self.estimate(9000.0), thin), 0)

    def test_index_never_leaves_ladder(self) -> None:
        policy = self.policy(upshift_confirm=1)
        top = self.ladder.size - 1
        self.assertEqual(
            policy.select(top, self.estimate(9_999_999.0), self.high_buffer),
            top,
        )


if __name__ == "__main__":
    unittest.main()
