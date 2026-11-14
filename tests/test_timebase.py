from fractions import Fraction

import pytest

from tlcore.timebase import (
    TimebaseError,
    frames_for_duration,
    ns_to_fraction,
    ns_to_seconds,
    parse_rate,
    parse_time,
    parse_v1_time,
    parse_v2_time,
    rational_seconds,
    seconds_to_ns,
)


def test_nanosecond_fraction_round_trip():
    assert ns_to_seconds(1_500_000_000) == Fraction(3, 2)
    assert ns_to_fraction(500_000_000) == Fraction(1, 2)
    assert seconds_to_ns(Fraction(3, 2)) == 1_500_000_000
    assert seconds_to_ns(Fraction(1, 3)) == 333_333_333


def test_v1_time_forms():
    assert parse_v1_time(42) == 42
    assert parse_v1_time("42ns") == 42
    assert parse_v1_time({"ns": 42}) == 42
    assert parse_time(42, version=1) == 42


def test_v2_time_forms_and_frames():
    assert parse_v2_time({"num": 3, "den": 2}) == 1_500_000_000
    assert parse_v2_time("3/2s") == 1_500_000_000
    assert parse_v2_time({"frame": 25}, fps=25) == 1_000_000_000
    assert parse_v2_time("50f", fps=25) == 2_000_000_000
    assert parse_v2_time(25, fps=25) == 1_000_000_000
    assert parse_v2_time(Fraction(1, 2)) == 500_000_000


def test_rates_are_exact_and_positive():
    assert parse_rate("30000/1001") == Fraction(30000, 1001)
    assert parse_rate({"num": 24, "den": 1}) == 24
    with pytest.raises(TimebaseError):
        parse_rate(0)
    with pytest.raises(TimebaseError):
        parse_rate("1/0")


def test_rejects_zero_denominator_and_floats():
    with pytest.raises(TimebaseError, match="denominator"):
        rational_seconds(1, 0)
    with pytest.raises(TimebaseError, match="float"):
        parse_time(1.5, version=2)
    with pytest.raises(TimebaseError, match="float"):
        seconds_to_ns(1.5)


def test_frame_count_uses_ceiling():
    assert frames_for_duration(0, 25) == 0
    assert frames_for_duration(1, 25) == 1
    assert frames_for_duration(40_000_000, 25) == 1
    assert frames_for_duration(2_000_000_000, 25) == 50
    assert frames_for_duration(1_000_000_000, Fraction(30000, 1001)) == 30
