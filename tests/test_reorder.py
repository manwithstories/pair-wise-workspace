"""Tests for bounded decode-to-display reordering."""

from __future__ import annotations

from cinecheck.decode import DecodedFrame
from cinecheck.reorder import (
    DEFAULT_REORDER_DEPTH,
    reorder_frames,
)


def _frame(order: int, pts, dts=None, pict="P", keyframe=False) -> DecodedFrame:
    return DecodedFrame(
        order=order,
        pts=pts,
        dts=order * 100 if dts is None else dts,
        keyframe=keyframe,
        pict_type=pict,
        packet_index=order,
    )


def test_ipb_decode_order_becomes_display_order():
    # Decode order I, P, B, B must come out as I, B, B, P by pts.
    frames = [
        _frame(0, 300, 0, "I", True),
        _frame(1, 900, 100, "P"),
        _frame(2, 500, 200, "B"),
        _frame(3, 700, 300, "B"),
    ]
    result = reorder_frames(frames)
    assert [frame.pts for frame in result.frames] == [300, 500, 700, 900]
    assert [frame.pict_type for frame in result.frames] == ["I", "B", "B", "P"]
    assert result.order_violations == 0


def test_missing_pts_is_inferred_from_dts():
    frames = [
        _frame(0, None, 40, "I", True),
        _frame(1, None, 80, "P"),
        _frame(2, None, 60, "B"),
    ]
    result = reorder_frames(frames)
    assert result.inferred_pts == 3
    assert [frame.pts for frame in result.frames] == [40, 60, 80]
    assert all(frame.pts_inferred for frame in result.frames)


def test_window_depth_is_clamped_to_32():
    frames = [_frame(i, i * 100) for i in range(100)]
    result = reorder_frames(frames, depth=10_000)
    assert result.window_peak <= DEFAULT_REORDER_DEPTH
    assert len(result.frames) == 100


def test_depth_one_preserves_decode_order_and_flags_regression():
    # A depth of one flushes immediately; a later smaller pts is a violation.
    frames = [_frame(i, pts) for i, pts in enumerate([0, 100, 50, 200])]
    result = reorder_frames(frames, depth=1)
    assert [frame.pts for frame in result.frames] == [0, 100, 50, 200]
    assert result.order_violations == 1


def test_stable_sort_keeps_equal_pts_in_decode_order():
    frames = [
        _frame(0, 100, 0, "I", True),
        _frame(1, 100, 100, "P"),
        _frame(2, 200, 200, "B"),
    ]
    result = reorder_frames(frames)
    assert [frame.packet_index for frame in result.frames] == [0, 1, 2]
