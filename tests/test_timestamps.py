from __future__ import annotations

from frameaudit.model import DecodedFrame, PresentedFrame
from frameaudit.timestamps import TimelineRepairer, median_frame_interval_us


def presented(
    index: int,
    pts: int | None,
    *,
    delta: int | None = None,
    segment: int = 0,
    missing: bool = False,
) -> PresentedFrame:
    source = DecodedFrame(
        decode_idx=index,
        pts_raw_us=pts,
        pict_type="P",
        keyframe=False,
        plane_hash=None,
        frame=None,
        missing_pts=missing,
        segment=segment,
    )
    return PresentedFrame(index, source, pts, missing, segment, delta)


def test_median_frame_interval_ignores_zero_and_negative() -> None:
    assert median_frame_interval_us([0, 40_000, 80_000, 80_000, 120_000], 1) == 40_000


def test_duplicate_and_rollback_are_classified_and_fixed_is_monotonic() -> None:
    repairer = TimelineRepairer(clock_us=40_000)
    frames = [
        presented(0, 0),
        presented(1, 40_000, delta=40_000),
        presented(2, 40_000, delta=0),
        presented(3, 0, delta=-40_000),
    ]
    repaired = [repairer.fix(item) for item in frames]

    assert [item.category for item in repaired] == [None, None, "duplicate_pts", "pts_rollback"]
    assert [item.pts_fixed_us for item in repaired] == [0, 40_000, 80_000, 120_000]
    assert repairer.summary()["fixed_monotonic"] is True


def test_gap_segment_jump_and_missing_pts_are_distinct() -> None:
    repairer = TimelineRepairer(clock_us=40_000, cross_jump_us=500_000)
    frames = [
        presented(0, 0),
        presented(1, 200_000, delta=200_000),
        presented(2, 2_000_000, delta=1_800_000),
        presented(3, 2_040_000, delta=40_000, missing=True),
    ]
    repaired = [repairer.fix(item) for item in frames]

    assert [item.category for item in repaired] == [None, "gap", "segment_jump", "missing_pts"]
    assert repairer.repaired_count == 3
    assert repairer.max_jump_before_us == 1_800_000
    assert repairer.max_jump_after_us == 40_000


def test_segment_change_is_classified_even_with_small_pts_delta() -> None:
    repairer = TimelineRepairer(clock_us=40_000)
    first = presented(0, 0, segment=0)
    second = presented(1, 40_000, segment=1)
    repairer.fix(first)
    repaired = repairer.fix(second)
    assert repaired.category == "segment_jump"

