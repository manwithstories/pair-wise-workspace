from fractions import Fraction

import pytest

from tlcore.timebase import (
    TimebaseError,
    format_clock,
    frames_to_samples,
    ms_to_samples,
    parse_samples,
    samples_to_bankers_ms,
    samples_to_frames,
    seconds_to_samples,
)


def test_sample_grid_and_clock():
    assert seconds_to_samples(1) == 48_000
    assert ms_to_samples("1500") == 72_000
    assert parse_samples({"value": 1, "rate": 2}) == 24_000
    assert format_clock(48_000) == "00:00:01.000000000"


def test_fractional_frame_rate_roundtrip():
    fps = Fraction(30_000, 1_001)
    assert frames_to_samples(1001, fps) == 1_603_202
    assert round(samples_to_frames(1_603_202, fps)) == 1_001


def test_bankers_rounding():
    assert samples_to_bankers_ms(24) == 0
    assert samples_to_bankers_ms(72) == 2
    assert parse_samples("33.5ms") == 1608


def test_invalid_time_and_rate():
    with pytest.raises(TimebaseError):
        parse_samples({"samples": 1.5})
    with pytest.raises(TimebaseError):
        frames_to_samples(1, 0)
