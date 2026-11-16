from __future__ import annotations

from fractions import Fraction

import pytest

from media_cli.packets import PacketRecord
from media_cli.reorder import ReorderError, reorder_packets
from media_cli.timestamps import TimestampRepairError, repair_timestamps


def packet(index: int, dts: int | None, pts: int | None, keyframe: bool = False) -> PacketRecord:
    return PacketRecord(index=index, dts=dts, pts=pts, keyframe=keyframe)


def test_reorder_uses_pts_inside_gop() -> None:
    packets = [
        packet(0, 0, 0, True),
        packet(1, 1, 3),
        packet(2, 2, 1),
        packet(3, 3, 2),
    ]
    ordered = reorder_packets(packets)
    assert [item.pts for item in ordered] == [0, 1, 2, 3]
    assert [item.index for item in ordered] == [0, 2, 3, 1]


def test_reorder_rejects_oversized_window() -> None:
    packets = [packet(index, index, index) for index in range(10)]
    with pytest.raises(ReorderError):
        reorder_packets(packets, window_size=6)


def test_reorder_expands_wrap_before_sorting() -> None:
    modulus = 1 << 32
    packets = [
        packet(0, 0, 5, True),
        packet(1, 1, modulus - 10),
        packet(2, 2, 15),
    ]
    ordered = reorder_packets(packets)
    assert [item.pts for item in ordered] == [modulus - 10, 5, 15]


def test_missing_pts_keeps_stable_display_position() -> None:
    packets = [packet(0, 0, 0, True), packet(1, 10, None), packet(2, 20, 20)]
    ordered = reorder_packets(packets)
    assert [item.index for item in ordered] == [0, 1, 2]


def test_wrap_is_expanded_then_zeroed() -> None:
    modulus = 1 << 32
    packets = [
        packet(0, 0, modulus - 10, True),
        packet(1, 10, 5),
        packet(2, 20, 15),
    ]
    result = repair_timestamps(packets, Fraction(1, 1000))
    assert [item.pts for item in result.packets] == [0, 15, 25]
    assert any(action.action == "wrap_expand" for action in result.actions)


def test_missing_pts_is_inferred_from_dts_and_median() -> None:
    packets = [packet(0, 0, 0, True), packet(1, 10, None), packet(2, 20, 20)]
    result = repair_timestamps(packets, Fraction(1, 1000))
    assert [item.pts for item in result.packets] == [0, 10, 20]
    assert any(action.action == "infer_missing_pts" for action in result.actions)


def test_duplicate_pts_is_advanced_by_median_interval() -> None:
    packets = [packet(0, 0, 0), packet(1, 10, 10), packet(2, 20, 10), packet(3, 30, 30)]
    result = repair_timestamps(packets, Fraction(1, 1000))
    assert [item.pts for item in result.packets] == [0, 10, 20, 30]
    assert any(action.action == "deduplicate_pts" for action in result.actions)
    assert result.max_jump_ms_after == 0.0


def test_first_frame_offset_is_recorded_and_zeroed() -> None:
    packets = [packet(0, 100, 100), packet(1, 110, 110), packet(2, 120, 120)]
    result = repair_timestamps(packets, Fraction(1, 1000))
    assert result.offset == 100
    assert [item.pts for item in result.packets] == [0, 10, 20]
    assert any(action.action == "zero_origin" for action in result.actions)


def test_no_timestamp_source_is_unrecoverable() -> None:
    packets = [packet(0, None, None), packet(1, None, None)]
    with pytest.raises(TimestampRepairError, match="neither PTS nor DTS"):
        repair_timestamps(packets, Fraction(1, 1000))
