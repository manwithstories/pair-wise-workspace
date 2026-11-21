"""Segment planning, keyframe conflicts and cross-rendition alignment."""

from __future__ import annotations

import unittest
from fractions import Fraction

from avtimeline.model import Rate, Span
from avtimeline.segment import AUDIO_BLOCK, plan_segments, sample_index_for, frame_index_for


class BoundaryTests(unittest.TestCase):
    def test_boundaries_shared_and_exact(self) -> None:
        plan = plan_segments(Span.of(0, Fraction(101, 50)), Rate(25), 48000, Fraction(2, 5))
        self.assertGreaterEqual(len(plan.segments), 6)
        self.assertEqual(plan.max_boundary_error(), Fraction(0))
        starts = [s.start for s in plan.segments]
        self.assertEqual(starts, sorted(starts))
        for segment in plan.segments:
            self.assertEqual(
                Fraction(segment.video_start_frame) * Fraction(1, 25), segment.start
            )
            self.assertEqual(
                Fraction(segment.audio_start_sample, 48000), segment.start
            )

    def test_ntsc_rate_audio_error_is_sub_sample(self) -> None:
        # NTSC frame times are not multiples of one 48 kHz sample, so the audio
        # representation can only be within half a sample of the frame time.
        rate = Rate.parse("30000/1001")
        plan = plan_segments(Span.of(0, Fraction(2)), rate, 48000, Fraction(2, 5))
        self.assertLessEqual(plan.max_boundary_error(), Fraction(1, 48000) / 2)
        for segment in plan.segments:
            expected = Fraction(segment.video_start_frame) * Fraction(1001, 30000)
            self.assertEqual(expected, segment.start)

    def test_short_tail_preserved_with_real_duration(self) -> None:
        window = Span.of(0, Fraction(101, 50))
        plan = plan_segments(
            window,
            Rate(25),
            48000,
            Fraction(2, 5),
            video_duration=Fraction(2),
            audio_duration=Fraction(101, 50),
        )
        tail = plan.short_segments
        self.assertEqual(len(tail), 1)
        self.assertEqual(tail[0].start, Fraction(2))
        self.assertEqual(tail[0].duration, Fraction(1, 50))
        self.assertEqual(tail[0].video_frames, 0)
        self.assertEqual(tail[0].audio_samples, 960)
        self.assertFalse(tail[0].has_video)
        self.assertTrue(tail[0].has_audio)

    def test_sub_block_tail_marked(self) -> None:
        tail = Fraction(AUDIO_BLOCK // 2, 48000)
        plan = plan_segments(
            Span.of(0, Fraction(2) + tail),
            Rate(25),
            48000,
            Fraction(2, 5),
            video_duration=Fraction(2),
            audio_duration=Fraction(2) + tail,
        )
        tail = plan.short_segments[-1]
        self.assertLess(tail.audio_samples, AUDIO_BLOCK)
        self.assertTrue(tail.is_short)
        self.assertEqual(tail.kind, "short_tail")

    def test_keyframe_conflict_position(self) -> None:
        plan = plan_segments(
            Span.of(0, Fraction(2)),
            Rate(25),
            48000,
            Fraction(2, 5),
            keyframe_grid=[Fraction(0), Fraction(1)],
        )
        self.assertTrue(plan.conflicts)
        conflict = plan.conflicts[0]
        self.assertEqual(conflict.reason, "keyframe_distance")
        self.assertEqual(conflict.time, Fraction(2, 5))

    def test_keyframe_grid_satisfied(self) -> None:
        plan = plan_segments(
            Span.of(0, Fraction(4, 5)),
            Rate(25),
            48000,
            Fraction(2, 5),
            keyframe_grid=[Fraction(0), Fraction(2, 5), Fraction(4, 5)],
        )
        self.assertEqual(plan.conflicts, [])

    def test_transition_guard_shifts_cut(self) -> None:
        plan = plan_segments(
            Span.of(0, Fraction(2)),
            Rate(25),
            48000,
            Fraction(2, 5),
            transitions=[Span.of(Fraction(3, 10), Fraction(1, 2))],
        )
        self.assertTrue(plan.guards)
        self.assertTrue(any(s.start == Fraction(12, 25) for s in plan.segments))

    def test_indices_round_to_nearest(self) -> None:
        self.assertEqual(sample_index_for(Fraction(1, 48000), 48000), 1)
        # Half a frame is a tie; it rounds to the even index.
        self.assertEqual(frame_index_for(Fraction(1, 50), Rate(25)), 0)
        self.assertEqual(frame_index_for(Fraction(3, 50), Rate(25)), 2)

    def test_empty_window(self) -> None:
        plan = plan_segments(Span.of(0, 0), Rate(25), 48000, Fraction(2, 5))
        self.assertEqual(plan.segments, [])


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
