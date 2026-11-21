"""Rational time base, spans and structured errors."""

from __future__ import annotations

import unittest
from fractions import Fraction

from avtimeline.errors import (
    ErrorCollector,
    ExitCode,
    MediaError,
    MediaLocation,
    Severity,
    Stage,
)
from avtimeline.model import (
    Rate,
    Span,
    TimeBase,
    TimeError,
    as_fraction,
    format_time,
    gaps_within,
    merge_spans,
    snap,
)


class FractionTests(unittest.TestCase):
    def test_int_and_string_accepted(self) -> None:
        self.assertEqual(as_fraction(3), Fraction(3))
        self.assertEqual(as_fraction("7/3"), Fraction(7, 3))

    def test_float_rejected(self) -> None:
        with self.assertRaises(TimeError):
            as_fraction(0.1)

    def test_bool_rejected(self) -> None:
        with self.assertRaises(TimeError):
            as_fraction(True)


class RateTests(unittest.TestCase):
    def test_parse_ntsc(self) -> None:
        rate = Rate.parse("30000/1001")
        self.assertEqual(rate.value, Fraction(30000, 1001))
        self.assertEqual(rate.frame_duration, Fraction(1001, 30000))

    def test_zero_denominator_rejected(self) -> None:
        with self.assertRaises(TimeError):
            Rate(1, 0)

    def test_snap_half_to_even(self) -> None:
        step = Fraction(1, 25)
        self.assertEqual(snap(Fraction(1, 50), step), Fraction(0))
        self.assertEqual(snap(Fraction(3, 50), step), step)


class SpanTests(unittest.TestCase):
    def test_merge_touching(self) -> None:
        merged = merge_spans([Span.of(0, 1), Span.of(1, 2), Span.of(4, 5)])
        self.assertEqual(merged, [Span.of(0, 2), Span.of(4, 5)])

    def test_gaps_within(self) -> None:
        gaps = gaps_within([Span.of(0, 1), Span.of(2, 3)], Span.of(0, 4))
        self.assertEqual(gaps, [Span.of(1, 2), Span.of(3, 4)])

    def test_intersection_empty(self) -> None:
        self.assertIsNone(Span.of(0, 1).intersection(Span.of(1, 2)))

    def test_end_before_start_rejected(self) -> None:
        with self.assertRaises(TimeError):
            Span.of(2, 1)


class TimeBaseTests(unittest.TestCase):
    def test_floor_and_ceil(self) -> None:
        tb = TimeBase(48000)
        self.assertEqual(tb.floor_ticks(Fraction(1, 3)), 16000)
        self.assertEqual(tb.ceil_ticks(Fraction(1, 3)), 16000)
        self.assertEqual(tb.floor_ticks(Fraction(1, 48000)), 1)

    def test_format(self) -> None:
        self.assertEqual(format_time(Fraction(3723, 1) + Fraction(1, 2)), "01:02:03.500")


class ErrorCollectorTests(unittest.TestCase):
    def test_first_error_is_earliest_media_time(self) -> None:
        collector = ErrorCollector()
        collector.add(
            MediaError(
                code="late",
                stage=Stage.ENCODE,
                message="late",
                location=MediaLocation(track="b", pts=Fraction(2)),
            )
        )
        collector.add(
            MediaError(
                code="early",
                stage=Stage.PROBE,
                message="early",
                location=MediaLocation(track="a", pts=Fraction(1), frame=25),
            )
        )
        summary = collector.summary()
        self.assertEqual(summary["first_error_track"], "a")
        self.assertEqual(summary["first_error_pts"], "1")
        self.assertEqual(summary["first_error_frame"], 25)

    def test_error_never_contains_payload(self) -> None:
        error = MediaError(
            code="decode_failed",
            stage=Stage.PROBE,
            message="cannot decode",
            location=MediaLocation(track="cam", sample_start=10, sample_end=20),
        )
        payload = error.to_dict()
        self.assertEqual(payload["sample_start"], 10)
        self.assertNotIn("payload", payload)

    def test_roundtrip_from_dict(self) -> None:
        original = MediaError(
            code="truncated_stream",
            stage=Stage.PROBE,
            message="short",
            location=MediaLocation(track="cam", pts=Fraction(6, 5), frame=30),
            severity=Severity.ERROR,
        )
        restored = MediaError.from_dict(original.to_dict())
        self.assertEqual(restored.sort_key(), original.sort_key())
        self.assertEqual(restored.location.frame, 30)

    def test_exit_codes(self) -> None:
        self.assertEqual(int(ExitCode.OK), 0)
        self.assertEqual(int(ExitCode.INVALID_INPUT), 2)
        self.assertEqual(int(ExitCode.VERIFY_FAILED), 3)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
