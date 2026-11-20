"""Timebase rounding, drift and boundary exactness."""

from __future__ import annotations

from fractions import Fraction

import pytest

from mediacore.errors import ParameterError
from mediacore.timebase import (
    DriftMeter,
    Timebase,
    align_us,
    round_half_away,
    samples_for_duration,
)


def test_round_half_away_is_symmetric():
    assert round_half_away(Fraction(1, 2)) == 1
    assert round_half_away(Fraction(-1, 2)) == -1
    assert round_half_away(Fraction(3, 2)) == 2
    assert round_half_away(Fraction(-3, 2)) == -2
    assert round_half_away(Fraction(2, 3)) == 1
    assert round_half_away(Fraction(-2, 3)) == -1


def test_ticks_to_us_never_truncates():
    # 1/15360 s = 65.104... us.  Truncation would walk backwards over a file.
    tb = Timebase.parse("1/15360")
    assert tb.to_us(1) == 65
    assert tb.to_us(512) == 33333


def test_timebase_roundtrip_is_stable_on_the_tick_grid():
    tb = Timebase.parse("1/44100")
    for tick in (0, 1, 1024, 44100, 10_000_000):
        us = tb.to_us(tick)
        assert tb.from_us(us) == tick


def test_parsing_accepts_common_forms():
    assert Timebase.parse("1/48000") == Timebase(1, 48000)
    assert Timebase.parse("48000") == Timebase(1, 48000)
    assert Timebase.parse(Fraction(2, 30000)).fraction == Fraction(2, 30000)
    with pytest.raises(ParameterError):
        Timebase.parse("0/1")
    with pytest.raises(ParameterError):
        Timebase.parse("nonsense")


def test_drift_over_ten_minutes_stays_within_a_microsecond():
    tb = Timebase.parse("1/15360")
    meter = DriftMeter()
    ticks = range(0, 15360 * 600, 512)  # 30 fps for 10 minutes
    align_us(ticks, tb, meter=meter)
    assert meter.max_abs_us <= 1
    assert meter.to_dict()["conversions"] == len(range(0, 15360 * 600, 512))


def test_align_handles_negative_and_nonzero_origins():
    tb = Timebase.parse("1/48000")
    values = align_us([-48000, 0, 48000], tb)
    assert values == [-1_000_000, 0, 1_000_000]
    rebased = align_us([96_000, 144_000], tb, origin_us=1_000_000)
    assert rebased == [1_000_000, 2_000_000]


def test_align_unwraps_rtp_counter():
    tb = Timebase.parse("1/48000")
    modulus = 2**32
    # Alignment is relative to the first sample, which establishes the base.
    raw = [modulus - 48000, modulus - 1, modulus, modulus + 48000]
    values = align_us(raw, tb, wrap_at=modulus)
    deltas = [b - a for a, b in zip(values, values[1:])]
    exact = round_half_away(Fraction(47999 * 1_000_000, 48000))
    assert abs(deltas[0] - exact) <= 1
    # The wrap from modulus-1 to modulus must not produce a huge negative jump.
    assert all(delta > 0 for delta in deltas)


def test_samples_for_duration_is_deterministic_and_symmetric():
    assert samples_for_duration(1_000_000, 48000) == 48000
    assert samples_for_duration(1, 48000) == 0
    assert samples_for_duration(10, 48000) == 0
    assert samples_for_duration(11, 48000) == 1  # 0.528 -> 1
    assert samples_for_duration(21, 48000) == 1  # 1.008 -> 1
    assert samples_for_duration(31, 48000) == 1  # 1.488 -> 1
    assert samples_for_duration(32, 48000) == 2  # 1.536 -> 2
    with pytest.raises(ParameterError):
        samples_for_duration(1_000_000, 0)
