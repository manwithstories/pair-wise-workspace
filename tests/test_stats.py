from fractions import Fraction

import pytest

from media_probe.framewalk import FrameRecord
from media_probe.repair import RepairRow, repair_timeline
from media_probe.reorder import display_order
from media_probe.stats import frame_type_counts, gop_lengths, rate_analysis

TB = Fraction(1, 15360)
STEP = 512


def rec(index, pts, keyframe=False, pict=None, dts=None):
    return FrameRecord(
        packet_index=index,
        pts=pts,
        dts=pts if dts is None else dts,
        duration=STEP,
        size=100,
        keyframe=keyframe,
        pict_type=pict,
    )


def rows_from_pts(pts_list):
    records = display_order([rec(i, p, pict="P") for i, p in enumerate(pts_list)])
    return repair_timeline(records, time_base=TB).rows


def test_frame_type_counts_direct():
    records = [rec(0, 0, pict="I"), rec(1, 512, pict="P"), rec(2, 1024, pict="B")]
    counts = frame_type_counts(records)
    assert counts == {"i_frames": 1, "p_frames": 1, "b_frames": 1, "pict_type_inferred": 0}


def test_frame_type_inference_by_keyframe_and_reference():
    # decode order: I, P(new max), B(pts below max), unknown-pts -> P
    records = [
        rec(0, 0, keyframe=True),
        rec(1, 1024),
        rec(2, 512),
        rec(3, None),
    ]
    counts = frame_type_counts(records)
    assert counts["pict_type_inferred"] == 4
    assert counts["i_frames"] == 1
    assert counts["p_frames"] == 2
    assert counts["b_frames"] == 1


def test_gop_lengths_between_keyframes():
    pts = [i * STEP for i in range(10)]
    records = display_order(
        [rec(i, p, keyframe=(i in (0, 4, 8)), pict="I" if i in (0, 4, 8) else "P")
         for i, p in enumerate(pts)]
    )
    rows = repair_timeline(records, time_base=TB).rows
    assert gop_lengths(rows) == [4, 4, 2]


def test_rate_analysis_cfr():
    rows = rows_from_pts([i * STEP for i in range(200)])
    rate = rate_analysis(rows, TB)
    assert rate["nominal_fps"] == 30.0
    assert rate["vfr"] is False
    assert rate["vfr_score"] == 0.0
    assert rate["confidence"] == 1.0


def test_rate_analysis_vfr():
    pattern = [33, 33, 67, 17, 50, 33, 100, 33, 25, 42]
    pts = [0]
    for delta in pattern * 20:
        pts.append(pts[-1] + delta)
    rows = rows_from_pts(pts)
    rate = rate_analysis(rows, Fraction(1, 1000))
    assert rate["vfr"] is True
    assert rate["vfr_score"] > 0.5
    assert rate["nominal_fps"] == pytest.approx(1000 / 33, abs=0.01)


def test_rate_analysis_degenerate():
    assert rate_analysis([], TB)["nominal_fps"] is None
    rows = rows_from_pts([0, 0, 0])  # collapses to 1-tick steps -> insane fps
    rate = rate_analysis(rows, TB)
    assert rate["nominal_fps"] is None
    assert rate["confidence"] == 0.0
