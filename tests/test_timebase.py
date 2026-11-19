"""Unit tests for the shared integer time base and error model."""

from __future__ import annotations

from fractions import Fraction

import pytest

from aircheck.timebase import (
    TICKS_PER_SECOND,
    AircheckError,
    CancelledError,
    ExitCode,
    InputError,
    MappingError,
    TimestampTracker,
    UsageError,
    WRAP_THRESHOLD,
    as_fraction,
    format_ticks,
    rescale_ticks,
    segment_index,
    to_ticks,
)


def test_to_ticks_is_exact_for_rational_time_bases():
    # 1 tick at 1/48000 s is 20833.33... ns; integer division truncates to 20833.
    assert to_ticks(1, Fraction(1, 48000)) == 20833
    assert to_ticks(48000, Fraction(1, 48000)) == TICKS_PER_SECOND
    assert to_ticks(None, Fraction(1, 1000)) is None


def test_to_ticks_avoids_float_drift_over_long_timelines():
    # Ten hours of 90 kHz ticks must land exactly on the integer nanosecond.
    ticks = 90_000 * 3600 * 10
    assert to_ticks(ticks, Fraction(1, 90000)) == 10 * 3600 * TICKS_PER_SECOND


def test_rescale_rounds_half_up():
    # 1/2 tick at 1/1000 s -> 1/2 microsecond -> rounds to 1 microsecond.
    assert rescale_ticks(1, Fraction(1, 1000), Fraction(1, 1_000_000)) == 1000
    assert rescale_ticks(3, Fraction(1, 2), Fraction(1, 1)) == 2
    assert rescale_ticks(5, Fraction(1, 8), Fraction(1, 2)) == 1


def test_as_fraction_accepts_tuples_and_ints():
    assert as_fraction((1, 25)) == Fraction(1, 25)
    assert as_fraction(4) == Fraction(4, 1)
    assert as_fraction(Fraction(3, 7)) == Fraction(3, 7)
    with pytest.raises(ValueError):
        as_fraction(None)


def test_format_ticks_handles_negative_and_none():
    assert format_ticks(None) == "none"
    assert format_ticks(1_500_000_000) == "1.500000000"
    assert format_ticks(-1_000_000_000) == "-1.000000000"


def test_segment_index_rejects_non_positive_length():
    assert segment_index(2_500_000_000, 1_000_000_000) == 2
    with pytest.raises(ValueError):
        segment_index(0, 0)


def test_error_categories_carry_stable_exit_codes():
    assert UsageError("x").exit_code == ExitCode.USAGE
    assert InputError("x").exit_code == ExitCode.INPUT
    assert MappingError("x").exit_code == ExitCode.INPUT
    assert CancelledError("x").exit_code == ExitCode.UNRECOVERABLE
    assert AircheckError("x").category.value == "internal"


def test_error_to_dict_keeps_context():
    error = MappingError("bad", stream_index=3, kind="video")
    payload = error.to_dict()
    assert payload["context"] == {"stream_index": 3, "kind": "video"}
    assert payload["category"] == "mapping"


class TestTimestampTracker:
    def test_monotonic_stream_reports_nothing(self):
        tracker = TimestampTracker(0, Fraction(1, 1000))
        for pts in range(0, 100, 10):
            assert tracker.observe(pts) == []
        assert tracker.issues == []
        assert tracker.report()["wraps"] == 0

    def test_backwards_small_step_is_non_monotonic(self):
        tracker = TimestampTracker(0, Fraction(1, 1000))
        tracker.observe(1000)
        issues = tracker.observe(900)
        assert [issue.kind for issue in issues] == ["non_monotonic"]
        assert issues[0].previous_pts == 1000

    def test_large_backwards_jump_to_zero_is_a_wrap(self):
        tracker = TimestampTracker(0, Fraction(1, 1000))
        high = WRAP_THRESHOLD + 5000
        tracker.observe(high)
        issues = tracker.observe(5)
        assert [issue.kind for issue in issues] == ["wrap"]
        assert tracker.wraps == 1

    def test_large_jump_not_landing_near_zero_is_disorder(self):
        tracker = TimestampTracker(0, Fraction(1, 1000))
        tracker.observe(WRAP_THRESHOLD * 3)
        issues = tracker.observe(WRAP_THRESHOLD * 2)
        assert [issue.kind for issue in issues] == ["non_monotonic"]

    def test_encoder_priming_negative_is_not_an_issue(self):
        tracker = TimestampTracker(1, Fraction(1, 48000))
        assert tracker.observe(-1024) == []
        assert tracker.observe(0) == []
        assert tracker.issues == []
        assert tracker.priming_packets == 1

    def test_negative_after_started_is_an_issue(self):
        tracker = TimestampTracker(1, Fraction(1, 48000))
        tracker.observe(0)
        issues = tracker.observe(-10)
        assert [issue.kind for issue in issues] == ["negative"]

    def test_none_pts_is_ignored(self):
        tracker = TimestampTracker(0, Fraction(1, 1000))
        assert tracker.observe(None) == []
        assert tracker.previous_raw is None

    def test_report_summarises_kinds(self):
        tracker = TimestampTracker(2, Fraction(1, 1000))
        tracker.observe(500)
        tracker.observe(400)
        tracker.observe(300)
        report = tracker.report()
        assert report["stream_index"] == 2
        assert report["kinds"] == ["non_monotonic"]
        assert report["issues"] == 2
