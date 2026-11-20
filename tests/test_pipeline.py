"""Pipeline, clock, manifest and policy tests.

Run with the project venv: ``.venv/bin/python -m pytest tests/ -q``
(or directly via ``python tests/test_pipeline.py``).
"""

from __future__ import annotations

import json
import sys
import unittest
from fractions import Fraction
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from relay_timeline.clock import DriftEstimator, WrapUnroller  # noqa: E402
from relay_timeline.clock import ticks_to_seconds  # noqa: E402
from relay_timeline.config import Limits, POLICY_FAIL, POLICY_SKIP  # noqa: E402
from relay_timeline.errors import (  # noqa: E402
    ErrorCode, FatalError, RecoverableError)
from relay_timeline.fixtures import FixtureSpec, build_fixture  # noqa: E402
from relay_timeline.manifest import parse_manifest  # noqa: E402
from relay_timeline.pipeline import (  # noqa: E402
    PipelineConfig, run_pipeline)

MOD = 8589934500


class ClockWrapTests(unittest.TestCase):
    def test_forward_wrap_expands_monotonically(self) -> None:
        u = WrapUnroller(MOD)
        near_top = MOD - 90_000
        first = u.unwrap(near_top)
        wrapped = u.unwrap(30_000)
        self.assertEqual(first, near_top)
        self.assertEqual(wrapped, MOD + 30_000)
        self.assertEqual(u.wraps, 1)
        self.assertGreater(wrapped, first)

    def test_classify_duplicate_and_backward(self) -> None:
        u = WrapUnroller(MOD)
        u.unwrap(1000)
        _, kind = u.classify(1000)
        self.assertEqual(kind, "duplicate")
        _, kind2 = u.classify(900)
        self.assertEqual(kind2, "backward")

    def test_rational_time_base_exact(self) -> None:
        self.assertEqual(ticks_to_seconds(90_000, Fraction(1, 90_000)),
                         Fraction(1))


class DriftTests(unittest.TestCase):
    def test_linear_drift_recovered_in_ppm(self) -> None:
        est = DriftEstimator(ppm_limit=50.0)
        for i in range(20):
            ref = Fraction(i)
            observed = ref * Fraction(1_000_085, 1_000_000)
            est.add(ref, observed)
        result = est.fit()
        self.assertIsNotNone(result)
        self.assertAlmostEqual(result.ppm, 85.0, delta=1.0)
        self.assertTrue(result.exceeded)

    def test_no_drift_within_limit(self) -> None:
        est = DriftEstimator(ppm_limit=100.0)
        for i in range(10):
            est.add(Fraction(i), Fraction(i))
        result = est.fit()
        self.assertIsNotNone(result)
        self.assertAlmostEqual(result.ppm, 0.0, places=6)
        self.assertFalse(result.exceeded)


class ManifestSecurityTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path("/tmp/rt-unit-manifest")
        self.tmp.mkdir(exist_ok=True)

    def test_path_traversal_rejected(self) -> None:
        m = self.tmp / "evil.m3u8"
        m.write_text("#EXTM3U\n#EXT-X-TARGETDURATION:2\n"
                     "#EXTINF:2.0,\n../../secret.ts\n#EXT-X-ENDLIST\n")
        with self.assertRaises(FatalError) as ctx:
            parse_manifest(m, Limits())
        self.assertEqual(ctx.exception.code, ErrorCode.PATH_TRAVERSAL)

    def test_absolute_path_rejected(self) -> None:
        m = self.tmp / "abs.m3u8"
        m.write_text("#EXTM3U\n#EXT-X-TARGETDURATION:2\n"
                     "#EXTINF:2.0,\n/tmp/secret.ts\n#EXT-X-ENDLIST\n")
        with self.assertRaises(FatalError):
            parse_manifest(m, Limits())


class PipelineEndToEndTests(unittest.TestCase):
    def _run(self, spec: FixtureSpec, **cfg) -> dict:
        import shutil
        if spec.out_dir.exists():
            shutil.rmtree(spec.out_dir)
        fixture = build_fixture(spec)
        result = run_pipeline(
            fixture.manifest, spec.out_dir / "run",
            PipelineConfig(queue_size=cfg.pop("queue_size", 4), **cfg),
            fixture.scenario)
        return result.summary

    def assertBetween(self, value, low, high) -> None:  # noqa: N802
        self.assertGreaterEqual(value, low)
        self.assertLessEqual(value, high)

    def test_clean_hls_passes_acceptance(self) -> None:
        stats = self._run(FixtureSpec(
            out_dir=Path("/tmp/rt-unit/clean"), fmt="hls", duration=12.0,
            drift_ppm=85.0, wrap_at=MOD, vfr=True))
        self.assertBetween(stats["frames"], 250, 400)
        self.assertLess(stats["bitrate_error_pct"], 8.0)
        self.assertLess(stats["manifest_alignment_ms"], 40.0)
        self.assertEqual(stats["frame_hashes"], stats["frames"])
        self.assertEqual(stats["error_counts"]["fatal"], 0)

    def test_wrap_counted_once(self) -> None:
        self._run(FixtureSpec(
            out_dir=Path("/tmp/rt-unit/wrap"), fmt="dash", duration=8.0,
            wrap_at=MOD))
        cp = json.loads(
            Path("/tmp/rt-unit/wrap/run/checkpoint.json").read_text())
        self.assertEqual(cp["wrap_count"], 1)

    def test_drift_estimated(self) -> None:
        stats = self._run(FixtureSpec(
            out_dir=Path("/tmp/rt-unit/drift"), fmt="hls", duration=8.0,
            drift_ppm=250.0))
        self.assertGreater(abs(stats["drift_ppm"]), 150.0)

    def test_truncated_gap_fill(self) -> None:
        stats = self._run(FixtureSpec(
            out_dir=Path("/tmp/rt-unit/trunc"), fmt="hls", duration=8.0,
            fault="truncated"))
        self.assertTrue(stats["gaps"])
        self.assertGreater(stats["gaps"][0]["frames_emitted"], 0)
        self.assertEqual(stats["error_counts"]["fatal"], 0)

    def test_missing_skip_emits_no_frames(self) -> None:
        stats = self._run(FixtureSpec(
            out_dir=Path("/tmp/rt-unit/skip"), fmt="dash", duration=8.0,
            fault="missing"), policy=POLICY_SKIP)
        self.assertTrue(stats["gaps"])
        self.assertEqual(stats["gaps"][0]["frames_emitted"], 0)

    def test_fail_policy_raises(self) -> None:
        spec = FixtureSpec(out_dir=Path("/tmp/rt-unit/fail"), fmt="hls",
                           duration=8.0, fault="zero")
        fixture = build_fixture(spec)
        with self.assertRaises(RecoverableError):
            run_pipeline(fixture.manifest, spec.out_dir / "run",
                         PipelineConfig(policy=POLICY_FAIL),
                         fixture.scenario)

    def test_resume_matches_clean_run(self) -> None:
        spec = FixtureSpec(out_dir=Path("/tmp/rt-unit/resume"), fmt="hls",
                           duration=8.0, vfr=True)
        fixture = build_fixture(spec)
        try:
            run_pipeline(fixture.manifest, spec.out_dir / "run",
                         PipelineConfig(abort_after=2), fixture.scenario)
        except RecoverableError:
            pass  # expected simulated crash; checkpoint is now on disk
        resumed = run_pipeline(fixture.manifest, spec.out_dir / "run",
                               PipelineConfig(), fixture.scenario)
        clean = self._run(FixtureSpec(
            out_dir=Path("/tmp/rt-unit/resume_ref"), fmt="hls",
            duration=8.0, vfr=True))
        for key in ("frames", "frame_hashes", "bitrate_actual_bps"):
            self.assertEqual(resumed.summary[key], clean[key])


if __name__ == "__main__":
    unittest.main(verbosity=2)
