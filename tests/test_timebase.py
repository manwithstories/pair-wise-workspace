from __future__ import annotations

from fractions import Fraction

import pytest

from cutplan.errors import CutplanError
from cutplan.timebase import (
    FrameInterval,
    FrameRate,
    RationalTime,
    frames_to_seconds,
    frames_to_timecode,
    parse_rate,
    parse_timecode,
    round_half_up,
    seconds_to_frames,
)


@pytest.mark.parametrize(
    "value, expected",
    [(24, "24"), (25, "25"), (30, "30"), ("30000/1001", "30000/1001"), ("29.97", "30000/1001")],
)
def test_parse_supported_rates(value: object, expected: str) -> None:
    assert str(parse_rate(value)) == expected


def test_invalid_rate_fails_loudly() -> None:
    for value in ("23.976", "60", 0, "abc", float("nan")):
        with pytest.raises(CutplanError, match="frame rate"):
            parse_rate(value)


def test_drop_frame_timecode_roundtrip() -> None:
    rate = parse_rate("30000/1001")
    for frame in (0, 1, 1798, 1799, 1800, 3598, 17982, 107892, 2589407):
        text = frames_to_timecode(frame, rate)
        assert parse_timecode(text, rate) == frame
    assert frames_to_timecode(1800, rate) == "00:01:00;02"
    assert frames_to_timecode(17982, rate) == "00:10:00;00"


def test_drop_frame_invalid_labels_and_rate() -> None:
    with pytest.raises(CutplanError, match="drop-frame"):
        parse_timecode("00:01:00;00", parse_rate("30000/1001"))
    with pytest.raises(CutplanError, match="drop-frame"):
        parse_timecode("00:00:01;00", parse_rate("25"))
    with pytest.raises(CutplanError):
        frames_to_timecode(5, parse_rate("25"), drop_frame=True)


def test_non_drop_timecode() -> None:
    rate = parse_rate("25")
    for frame in (0, 24, 25, 1499):
        text = frames_to_timecode(frame, rate)
        assert parse_timecode(text, rate) == frame
    assert frames_to_timecode(25, rate) == "00:00:01:00"


def test_half_up_and_seconds() -> None:
    assert round_half_up(Fraction(5, 2)) == 3
    assert round_half_up(4.5) == 5
    assert seconds_to_frames("0.5", 25) == 13
    assert frames_to_seconds(300, 25) == Fraction(12, 1)
    assert RationalTime(24, parse_rate("24")).rescale(parse_rate("25")).frames == 25


def test_half_open_interval() -> None:
    interval = FrameInterval(10, 20)
    assert interval.duration == 10
    assert interval.half_open == (10, 20)
    assert FrameInterval(20, 20).empty
    with pytest.raises(CutplanError, match="precede"):
        FrameInterval(20, 10)
    with pytest.raises(CutplanError):
        FrameInterval(-1, 2)
    assert FrameRate(25, 1) == parse_rate("25")
