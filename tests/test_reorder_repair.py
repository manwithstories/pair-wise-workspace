from fractions import Fraction

from media_probe.framewalk import FrameRecord
from media_probe.reorder import derive_missing_dts, display_order
from media_probe.repair import estimate_interval, repair_timeline

TB = Fraction(1, 90000)
STEP = 3000  # one 30fps frame at 1/90000


_AUTO = object()


def rec(index, pts, dts=_AUTO, keyframe=False, pict="P"):
    return FrameRecord(
        packet_index=index,
        pts=pts,
        dts=pts if dts is _AUTO else dts,
        duration=STEP,
        size=100,
        keyframe=keyframe,
        pict_type=pict,
    )


def test_display_order_sorts_by_pts_then_dts_then_packet():
    records = [rec(0, 2 * STEP), rec(1, STEP), rec(2, STEP), rec(3, 0)]
    ordered = display_order(records)
    assert [r.packet_index for r in ordered] == [3, 1, 2, 0]
    # ties keep packet order (stable)
    assert [r.pts for r in ordered] == [0, STEP, STEP, 2 * STEP]


def test_derive_missing_dts_fills_monotonic():
    records = [rec(0, 0, dts=None), rec(1, STEP, dts=None), rec(2, 2 * STEP, dts=None)]
    notes = derive_missing_dts(display_order(records), STEP)
    assert notes
    assert [r.dts for r in records] == [0, STEP, 2 * STEP]


def test_estimate_interval_uses_median_of_positive_deltas():
    records = display_order([rec(i, p) for i, p in enumerate([0, 0, 3000, 6000, 12000])])
    assert estimate_interval(records) == 3000


def test_repair_zero_shift_anchors_timeline():
    records = display_order([rec(i, 9000 + i * STEP) for i in range(4)])
    result = repair_timeline(records, time_base=TB)
    assert result.events["zero_shift"] == 1
    assert [row.pts for row in result.rows] == [0, STEP, 2 * STEP, 3 * STEP]


def test_repair_collapses_duplicate_pts_into_hole():
    # duplicated stamp with an adjacent hole, as produced by fixtures_gen
    pts = [0, 0, 2 * STEP, 3 * STEP, 4 * STEP, 5 * STEP]
    records = display_order([rec(i, p) for i, p in enumerate(pts)])
    result = repair_timeline(records, time_base=TB)
    assert result.events["duplicate_pts"] == 1
    assert "monotonize" not in result.events
    assert [row.pts for row in result.rows] == [i * STEP for i in range(6)]
    assert result.rows[1].repaired


def test_repair_fixes_dts_greater_than_pts():
    records = display_order([rec(0, 0, dts=STEP), rec(1, STEP, dts=STEP)])
    result = repair_timeline(records, time_base=TB)
    assert result.events["dts_gt_pts"] == 1
    assert result.rows[0].dts == result.rows[0].pts - result.interval_ticks


def test_repair_fixes_dts_regression_in_decode_order():
    records = display_order([rec(0, 0, dts=0), rec(1, STEP, dts=0), rec(2, 2 * STEP)])
    result = repair_timeline(records, time_base=TB)
    assert result.events["dts_regression"] == 1
    dts_in_decode_order = [
        row.dts for row in sorted(result.rows, key=lambda r: r.source.packet_index)
    ]
    assert dts_in_decode_order == sorted(dts_in_decode_order)


def test_jump_detection_threshold_configurable():
    pts = [0, STEP, 5 * STEP, 6 * STEP]  # one gap of 4 intervals
    records = display_order([rec(i, p) for i, p in enumerate(pts)])
    strict = repair_timeline(records, jump_threshold=1.5, time_base=TB)
    assert len(strict.jumps) == 1
    assert strict.max_jump_ms == 4 * STEP / 90.0
    loose = repair_timeline(records, jump_threshold=5.0, time_base=TB)
    assert not loose.jumps


def test_scan_and_repair_share_kernel_events():
    pts = [0, 0, STEP, 2 * STEP]
    records = display_order([rec(i, p) for i, p in enumerate(pts)])
    first = repair_timeline(list(records), time_base=TB)
    second = repair_timeline(list(records), time_base=TB)
    assert first.events == second.events
    assert [r.pts for r in first.rows] == [r.pts for r in second.rows]
