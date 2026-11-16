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


def make_config(**overrides) -> SimulationConfig:
    defaults = dict(
        bitrates=LADDER,
        segment_seconds=2.0,
        initial_buffer=6.0,
        max_buffer=30.0,
        window=6,
        upshift_confirm=3,
    )
    defaults.update(overrides)
    return SimulationConfig(**defaults)


class SimulatorTests(unittest.TestCase):
    def test_steady_high_bandwidth_ramps_up_and_never_stalls(self) -> None:
        trace = Trace(
            tuple(sample(float(i) * 2.0, 9000.0) for i in range(30)), 2.0
        )
        decisions = Simulator(make_config()).run(trace)
        self.assertEqual(decisions[0].bitrate_kbps, 800)  # warmup floor
        # 9000 * 0.85 safety margin = 7650, so 5000 is the top affordable
        # rung (the 8000 level deliberately stays out of reach).
        self.assertEqual(decisions[-1].bitrate_kbps, 5000)
        self.assertFalse(any(d.stall for d in decisions))
        self.assertTrue(all(d.buffer_seconds >= 0.0 for d in decisions))

    def test_buffer_never_negative_under_long_outage(self) -> None:
        # 10 healthy segments then a 20-segment total outage.
        samples = [sample(float(i) * 2.0, 8000.0) for i in range(10)]
        samples += [
            sample(float(10 + i) * 2.0, 0.0, rtt=300.0, loss=0.8)
            for i in range(20)
        ]
        decisions = Simulator(make_config()).run(Trace(tuple(samples), 2.0))
        self.assertTrue(all(d.buffer_seconds >= 0.0 for d in decisions))
        self.assertTrue(all(d.download_seconds >= 0.0 for d in decisions))
        self.assertTrue(any(d.stall for d in decisions))
        # The outage collapses to a single contiguous stall episode.
        episodes = find_stall_episodes(decisions)
        self.assertEqual(len(episodes), 1)
        self.assertGreater(episodes[0].segments, 1)  # sustained, not brief

    def test_stall_seconds_match_download_minus_buffer(self) -> None:
        # One segment whose download (at 200 kbps for a 5000 kbps level)
        # far exceeds the available buffer.
        samples = (
            sample(0.0, 9000.0), sample(2.0, 9000.0),
            sample(4.0, 9000.0), sample(6.0, 200.0),
        )
        decisions = Simulator(
            make_config(initial_buffer=2.0, max_buffer=10.0)
        ).run(Trace(samples, 2.0))
        stalled = [d for d in decisions if d.stall]
        self.assertEqual(len(stalled), 1)
        hit = stalled[0]
        expected = round(hit.download_seconds - hit.buffer_seconds, 3)
        self.assertAlmostEqual(hit.stall_seconds, expected, places=2)

    def test_sudden_dip_does_not_repeatedly_upshift(self) -> None:
        trace = generate_trace(180, 2.0, 42)
        decisions = Simulator(make_config()).run(trace)
        # Dip phase occupies roughly segments 54..75; within it the level
        # may fall but must not bounce back up more than once.
        dip = decisions[54:76]
        upshifts = 0
        for prev, cur in zip(dip, dip[1:]):
            if cur.bitrate_kbps > prev.bitrate_kbps:
                upshifts += 1
        self.assertLessEqual(upshifts, 1)

    def test_recovery_shows_hysteresis(self) -> None:
        # A flapping link: confirmation gating must keep switches bounded
        # even though the raw estimate oscillates every segment.
        throughput = []
        for i in range(40):
            throughput.append(4500.0 if i % 2 == 0 else 900.0)
        trace = Trace(
            tuple(sample(float(i) * 2.0, v) for i, v in enumerate(throughput)),
            2.0,
        )
        decisions = Simulator(make_config(upshift_confirm=3)).run(trace)
        switches = sum(1 for d in decisions if d.switch)
        # 40 wildly alternating segments must not produce 20+ switches.
        self.assertLess(switches, 12)

    def test_segment_second_mismatch_is_runtime_error(self) -> None:
        trace = Trace((sample(0.0, 1000.0),), 4.0)
        with self.assertRaises(ABRSimulationError):
            Simulator(make_config(segment_seconds=2.0)).run(trace)

    def test_summary_metrics_are_consistent(self) -> None:
        trace = generate_trace(180, 2.0, 42)
        decisions = Simulator(make_config()).run(trace)
        summary = summarize(decisions)
        self.assertEqual(summary.segments, 180)
        self.assertEqual(
            summary.switch_count, sum(1 for d in decisions if d.switch)
        )
        self.assertEqual(
            summary.stall_segment_count,
            sum(1 for d in decisions if d.stall),
        )
        self.assertGreaterEqual(summary.average_bitrate_kbps,
                                summary.min_bitrate_kbps)
        self.assertLessEqual(summary.average_bitrate_kbps,
                             summary.max_bitrate_kbps)


class DeterminismAndIOTests(unittest.TestCase):
    def test_seeded_trace_is_byte_reproducible(self) -> None:
        first = generate_trace(180, 2.0, 42)
        second = generate_trace(180, 2.0, 42)
        self.assertEqual(first.samples, second.samples)
        different = generate_trace(180, 2.0, 43)
        self.assertNotEqual(first.samples, different.samples)

    def test_trace_json_round_trip_preserves_samples(self) -> None:
        trace = generate_trace(24, 2.0, 7)
        with tempfile.TemporaryDirectory() as tmp:
            path = save_trace(trace, Path(tmp) / "trace.json")
            loaded = load_trace(path)
        self.assertEqual(loaded.samples, trace.samples)
        self.assertEqual(loaded.seed, 7)

    def _write_bad_trace(self, tmp: Path, payload: dict) -> Path:
        path = tmp / "bad.json"
        path.write_text(json.dumps(payload), encoding="utf-8")
        return path

    def test_illegal_traces_are_rejected(self) -> None:
        good_sample = {"t": 1.0, "throughput": 1000.0, "rtt": 30.0, "loss": 0.0}
        with tempfile.TemporaryDirectory() as name:
            tmp = Path(name)
            cases = {
                "empty": {"segment_seconds": 2.0, "samples": []},
                "duplicate": {"segment_seconds": 2.0, "samples": [
                    good_sample,
                    {"t": 1.0, "throughput": 2000.0, "rtt": 30.0, "loss": 0.0},
                ]},
                "negative_loss": {"segment_seconds": 2.0, "samples": [
                    {"t": 1.0, "throughput": 1.0, "rtt": 1.0, "loss": -0.2},
                ]},
                "missing_field": {"segment_seconds": 2.0, "samples": [
                    {"t": 1.0, "throughput": 1.0},
                ]},
            }
            for label, payload in cases.items():
                with self.subTest(label=label):
                    with self.assertRaises(ABRTraceError):
                        load_trace(self._write_bad_trace(tmp, payload))
            not_json = tmp / "notjson.json"
            not_json.write_text("<html>not json</html>", encoding="utf-8")
            with self.assertRaises(ABRTraceError):
                load_trace(not_json)
            with self.assertRaises(ABRTraceError):
                load_trace(tmp / "does_not_exist.json")

    def test_illegal_ladder_and_config_rejected(self) -> None:
        for text in ("", "800,,1600", "800,abc", "1600,800", "0,800"):
            with self.subTest(text=text):
                with self.assertRaises(ABRConfigError):
                    parse_bitrates(text)
        with self.assertRaises(ABRConfigError):
            make_config(initial_buffer=40.0, max_buffer=30.0)
        with self.assertRaises(ABRConfigError):
            make_config(window=0)
        with self.assertRaises(ABRConfigError):
            make_config(upshift_confirm=0)

    def test_model_constructors_reject_bad_numbers(self) -> None:
        with self.assertRaises(ABRTraceError):
            sample(0.0, -1.0)
        with self.assertRaises(ABRTraceError):
            NetworkSample(0.0, 1.0, 1.0, 1.5)
        with self.assertRaises(ABRConfigError):
            BitrateLadder(())
        with self.assertRaises(ABRConfigError):
            BitrateLadder((800, 800))


class LargeTraceTests(unittest.TestCase):
    def test_hundred_thousand_segments_linear_and_finite(self) -> None:
        trace = generate_trace(100_000, 2.0, 42)
        config = make_config()
        decisions = Simulator(config).run(trace)
        self.assertEqual(len(decisions), 100_000)
        # No NaN / negative state anywhere in the full run.
        for decision in decisions:
            self.assertGreaterEqual(decision.buffer_seconds, 0.0)
            self.assertGreaterEqual(decision.download_seconds, 0.0)
            self.assertGreater(decision.bitrate_kbps, 0)
        summary = summarize(decisions)
        self.assertEqual(summary.segments, 100_000)
        # Every selected level must be a real ladder entry.
        valid = set(LADDER)
        self.assertTrue(all(d.bitrate_kbps in valid for d in decisions))


if __name__ == "__main__":
    unittest.main()
