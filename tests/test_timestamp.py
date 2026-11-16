"""Tests for timestamp normalization and the auditability of the rewrite plan."""

from __future__ import annotations

import unittest

import numpy as np

from src.timestamp import (
    TimingConfig,
    TimestampError,
    build_timeline,
    monotonic_ok,
    plan_rewrite,
)
from tests.helpers import make_document


class BaselineTimelineTests(unittest.TestCase):
    def test_regular_video_keeps_shape(self) -> None:
        doc = make_document(
            [("video", 1, [0, 40, 80, 120]), ("audio", 2, [0, 21, 42, 63])],
            scale_ns=1_000_000,
        )
        plan = plan_rewrite(doc)
        video = plan.timeline(1)
        self.assertEqual(video.sample_count, 4)
        self.assertEqual(video.timescale, 90_000)
        self.assertEqual(list(video.rewritten_dts), [0, 3600, 7200, 10800])
        # Only the trailing sample lacks an explicit duration.
        self.assertEqual(video.stats.total(), 1)
        self.assertEqual(video.stats.missing_duration, 1)

    def test_audio_uses_audio_timescale(self) -> None:
        doc = make_document([("audio", 1, [0, 21, 42])], scale_ns=1_000_000)
        tl = plan_rewrite(doc).timeline(1)
        self.assertEqual(tl.timescale, 48_000)
        self.assertEqual(list(tl.rewritten_dts), [0, 1008, 2016])

    def test_non_default_timecode_scale(self) -> None:
        # 100 µs ticks are legal Matroska and must be normalized, not assumed 1 ms.
        doc = make_document([("video", 1, [0, 400, 800])], scale_ns=100_000)
        tl = plan_rewrite(doc).timeline(1)
        self.assertEqual(tl.source_scale_ns, 100_000)
        self.assertEqual(list(tl.rewritten_dts), [0, 3600, 7200])


class NegativeAndRepairTests(unittest.TestCase):
    def test_negative_origin_shifted_globally(self) -> None:
        doc = make_document(
            [("video", 1, [-100, -60, -20]), ("audio", 2, [-100, -79, -58])],
            scale_ns=1_000_000,
        )
        plan = plan_rewrite(doc)
        self.assertEqual(plan.global_shift_ticks, 100)
        video = plan.timeline(1)
        self.assertEqual(list(video.rewritten_dts), [0, 3600, 7200])
        audio = plan.timeline(2)
        self.assertEqual(list(audio.rewritten_dts), [0, 1008, 2016])
        self.assertEqual(video.stats.negative, 3)
        self.assertTrue(all(tl.first_rewritten_seconds() >= 0 for tl in plan.timelines.values()))

    def test_negative_without_normalization_rejected(self) -> None:
        doc = make_document([("video", 1, [-2, 0, 2])], scale_ns=1_000_000)
        config = TimingConfig(normalize_origin=False)
        with self.assertRaises(TimestampError) as ctx:
            plan_rewrite(doc, config=config)
        self.assertEqual(ctx.exception.code, "negative_timestamps")

    def test_duplicates_nudged_by_min_step(self) -> None:
        # The spec requires repeated values to be corrected by the smallest
        # legal increment, so the duplicate moves forward instead of collapsing.
        doc = make_document([("video", 1, [0, 40, 40, 80])], scale_ns=1_000_000)
        tl = plan_rewrite(doc).timeline(1)
        self.assertEqual(tl.stats.duplicates, 1)
        self.assertTrue(monotonic_ok(tl.rewritten_pts))
        self.assertEqual(int(tl.rewritten_pts[2] - tl.rewritten_pts[1]), 1)
        self.assertTrue(monotonic_ok(tl.rewritten_dts))

    def test_duplicates_rejected_when_repair_disabled(self) -> None:
        doc = make_document([("video", 1, [0, 40, 40])], scale_ns=1_000_000)
        with self.assertRaises(TimestampError) as ctx:
            plan_rewrite(doc, config=TimingConfig(repair_duplicates=False))
        self.assertEqual(ctx.exception.code, "duplicate_timestamps")

    def test_reordering_keeps_pts_and_grids_dts(self) -> None:
        # A backwards PTS is B-frame reordering, not corruption: PTS stays as
        # authored and DTS moves to a strictly increasing grid.
        doc = make_document([("video", 1, [0, 40, 20, 60])], scale_ns=1_000_000)
        tl = plan_rewrite(doc).timeline(1)
        self.assertEqual(tl.stats.regressions, 1)
        self.assertEqual(list(tl.rewritten_pts), [0, 3600, 1800, 5400])
        self.assertTrue(monotonic_ok(tl.rewritten_dts))

    def test_run_of_duplicates_becomes_distinct(self) -> None:
        doc = make_document([("video", 1, [0, 40, 40, 40, 120])], scale_ns=1_000_000)
        tl = plan_rewrite(doc).timeline(1)
        self.assertEqual(tl.stats.duplicates, 2)
        self.assertTrue(monotonic_ok(tl.rewritten_pts))

    def test_clamped_counter_visible_when_shift_is_skipped(self) -> None:
        # build_timeline is the lower layer: with no shift supplied, values
        # below zero are clamped so the result is still MP4-legal, and the
        # count is reported instead of the values silently collapsing.
        doc = make_document([("video", 1, [-100, -60, -20])], scale_ns=1_000_000)
        tl = build_timeline(doc, doc.tracks[0], shift_ns=0)
        self.assertEqual(tl.stats.clamped, 3)
        self.assertTrue(monotonic_ok(tl.rewritten_pts))

    def test_force_monotonic_mode_flattens_reordering(self) -> None:
        doc = make_document([("video", 1, [0, 40, 20])], scale_ns=1_000_000)
        tl = plan_rewrite(doc, config=TimingConfig(force_monotonic_pts=True)).timeline(1)
        self.assertTrue(monotonic_ok(tl.rewritten_pts))
        self.assertTrue(monotonic_ok(tl.rewritten_dts))

    def test_jump_counted_but_kept(self) -> None:
        doc = make_document([("video", 1, [0, 40, 4000, 4040])], scale_ns=1_000_000)
        tl = plan_rewrite(doc).timeline(1)
        self.assertEqual(tl.stats.jumps, 1)
        self.assertTrue(any("exceed" in note for note in tl.notes))


class DurationTests(unittest.TestCase):
    def test_durations_follow_next_dts(self) -> None:
        doc = make_document([("video", 1, [0, 40, 80])], scale_ns=1_000_000)
        tl = plan_rewrite(doc).timeline(1)
        self.assertEqual(list(tl.durations), [3600, 3600, 3600])
        self.assertAlmostEqual(tl.last_rewritten_seconds(), 0.12)

    def test_missing_duration_uses_nominal(self) -> None:
        doc = make_document([("video", 1, [0])], scale_ns=1_000_000, default_duration_ns=33_333_333)
        tl = plan_rewrite(doc).timeline(1)
        self.assertEqual(tl.stats.missing_duration, 1)
        self.assertEqual(int(tl.durations[0]), 3000)  # 33.333ms * 90k

    def test_no_duration_hint_falls_back(self) -> None:
        doc = make_document([("video", 1, [0])], scale_ns=1_000_000)
        tl = plan_rewrite(doc).timeline(1)
        self.assertIsNotNone(tl.nominal_duration_ns)
        self.assertTrue(tl.durations[0] > 0)


class PlanLevelTests(unittest.TestCase):
    def test_track_without_blocks_warns(self) -> None:
        doc = make_document(
            [("video", 1, [0, 40]), ("subtitle", 3, [])],
            scale_ns=1_000_000,
        )
        plan = plan_rewrite(doc)
        self.assertIn(1, plan.timelines)
        self.assertNotIn(3, plan.timelines)
        self.assertEqual(plan.warnings[0]["code"], "track_without_blocks")

    def test_no_samples_rejected(self) -> None:
        doc = make_document([("video", 1, [])], scale_ns=1_000_000)
        with self.assertRaises(TimestampError) as ctx:
            plan_rewrite(doc)
        self.assertEqual(ctx.exception.code, "no_samples")

    def test_intertrack_spread_warns(self) -> None:
        doc = make_document(
            [("video", 1, [0, 40]), ("audio", 2, [5000, 5021])],
            scale_ns=1_000_000,
        )
        plan = plan_rewrite(doc)
        self.assertTrue(any(w["code"] == "intertrack_origin_spread" for w in plan.warnings))

    def test_summary_is_json_ready(self) -> None:
        import json

        doc = make_document([("video", 1, [0, 40])], scale_ns=1_000_000)
        payload = json.dumps(plan_rewrite(doc).summary())
        self.assertIn("rewritten_first_pts", payload)

    def test_arrays_are_numpy(self) -> None:
        doc = make_document([("video", 1, [0, 40, 80])], scale_ns=1_000_000)
        tl = plan_rewrite(doc).timeline(1)
        for arr in (tl.source_ticks, tl.rewritten_dts, tl.durations):
            self.assertIsInstance(arr, np.ndarray)


if __name__ == "__main__":
    unittest.main()
