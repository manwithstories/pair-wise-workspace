"""共同切点与分片清单测试：对齐、偏差拒绝、未对齐原因可核对。"""

from fractions import Fraction as F

from mediasync.errors import ErrorBook, ErrorCode
from mediasync.segment import Representation, plan_cuts, plan_to_dict


def _rep(name: str, frames: list[int]) -> Representation:
    return Representation(name=name, keyframe_ms=[F(x) for x in frames])


def test_aligned_three_representations_produce_segments():
    book = ErrorBook()
    reps = [
        _rep("rep0", [0, 500, 1000]),
        _rep("rep1", [0, 502, 1001]),
        _rep("rep2", [0, 499, 1000]),
    ]
    plan = plan_cuts(reps, segment_ms=500, max_deviation_ms=10, errors=book)

    assert plan.fully_aligned
    assert plan.aligned_representations == ("rep0", "rep1", "rep2")
    assert len(plan.segments) >= 2
    assert plan.cuts[0].boundary_ms == F(500)
    assert plan.cuts[0].max_deviation_ms == F(2)
    assert plan.rejected == []
    # 分片时长为有理数且首尾相接。
    for prev, nxt in zip(plan.segments, plan.segments[1:]):
        assert prev.end_ms == nxt.start_ms


def test_cut_rejected_when_deviation_exceeds_threshold():
    book = ErrorBook()
    reps = [
        _rep("rep0", [0, 500, 1000]),
        _rep("rep1", [0, 640, 1000]),  # 偏差 140ms 超出 50ms 容差
    ]
    plan = plan_cuts(reps, segment_ms=500, max_deviation_ms=50, errors=book)

    assert not plan.fully_aligned
    assert book.has(ErrorCode.ALIGNMENT_IMPOSSIBLE)
    assert book.any_fatal()
    assert plan.segments == []
    reason = plan.rejected[0]
    assert "rep1" in reason.reason
    assert reason.per_rep_deviation["rep1"] == F(140)


def test_anchor_without_keyframe_in_window_rejected():
    book = ErrorBook()
    reps = [_rep("rep0", [0, 900]), _rep("rep1", [0, 500, 900])]
    plan = plan_cuts(reps, segment_ms=500, max_deviation_ms=20, errors=book)
    assert not plan.fully_aligned
    assert "锚点" in plan.rejected[0].reason


def test_empty_representation_is_alignment_impossible():
    book = ErrorBook()
    plan = plan_cuts([], segment_ms=500, max_deviation_ms=20, errors=book)
    assert book.has(ErrorCode.ALIGNMENT_IMPOSSIBLE)
    assert plan.total_representations == 0


def test_invalid_config_rejected():
    book = ErrorBook()
    plan = plan_cuts([_rep("r", [0, 1])], segment_ms=0, max_deviation_ms=5, errors=book)
    assert book.has(ErrorCode.CONFIG_ERROR)
    assert plan.segments == []


def test_plan_to_dict_uses_exact_rational_and_is_stable():
    book = ErrorBook()
    reps = [_rep("rep0", [0, 500, 1000]), _rep("rep1", [0, 501, 1000])]
    plan = plan_cuts(reps, segment_ms=500, max_deviation_ms=10, errors=book)
    data = plan_to_dict(plan)

    first = data["segments"][1]
    assert first["start_ms"] == {"num": 500, "den": 1}
    assert first["cuts"]["rep1"] == {"num": 501, "den": 1}
    # 重复规划输出完全一致（排序键固定）。
    assert plan_to_dict(plan) == data


def test_partial_alignment_reports_which_representations_aligned():
    book = ErrorBook()
    reps = [
        _rep("rep0", [0, 500, 1000]),
        _rep("rep1", [0, 500, 1000]),
        _rep("rep2", [0, 700, 1000]),  # 第一切点对不上
    ]
    plan = plan_cuts(reps, segment_ms=500, max_deviation_ms=20, errors=book)
    # 首切点被整体拒绝 -> 无可用切点，记录致命对齐失败，但原因可定位到 rep2。
    assert not plan.fully_aligned
    assert "rep2" in plan.rejected[0].reason
