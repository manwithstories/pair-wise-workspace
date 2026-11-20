"""Timeline dependency-graph and recovery-order tests."""

from __future__ import annotations

import pytest

from mediasync.errors import PlanError
from mediasync.timeline import (
    Dependency,
    Interval,
    Timeline,
    build_timeline,
    gaps_between,
)


def interval(name: str, start: int, end: int, track: str = "cam", **kwargs) -> Interval:
    return Interval(name, track, start, end, **kwargs)


def test_topological_order_respects_dependencies() -> None:
    head = interval("head", 0, 1000)
    middle = interval("middle", 1000, 2000, depends_on=("head",))
    tail = interval("tail", 2000, 3000, depends_on=("middle",))
    audio = interval("audio", 0, 3000, track="mic", kind="audio", depends_on=("head",))
    timeline = build_timeline([[head, middle, tail], [audio]])
    order = timeline.topological_order()
    assert order.index("head") < order.index("middle") < order.index("tail")
    assert order.index("head") < order.index("audio")


def test_cycles_are_rejected_with_the_cycle_path() -> None:
    first = interval("a", 0, 10, depends_on=("b",))
    second = interval("b", 10, 20, depends_on=("a",))
    with pytest.raises(PlanError) as excinfo:
        build_timeline([[first, second]])
    assert "cycle" in str(excinfo.value)
    assert "a" in str(excinfo.value) and "b" in str(excinfo.value)


def test_self_dependency_is_rejected() -> None:
    with pytest.raises(PlanError):
        build_timeline([[interval("solo", 0, 10, depends_on=("solo",))]])


def test_missing_dependency_is_rejected() -> None:
    with pytest.raises(PlanError) as excinfo:
        build_timeline([[interval("x", 0, 10, depends_on=("ghost",))]])
    assert "ghost" in str(excinfo.value)


def test_overlapping_intervals_on_one_track_are_rejected() -> None:
    with pytest.raises(PlanError) as excinfo:
        build_timeline([[interval("a", 0, 1000), interval("b", 500, 1500)]])
    assert "overlap" in str(excinfo.value)


def test_adjacent_intervals_are_allowed() -> None:
    timeline = build_timeline([[interval("a", 0, 1000), interval("b", 1000, 2000)]])
    assert timeline.topological_order() == ["a", "b"]


def test_same_span_on_different_tracks_is_allowed() -> None:
    timeline = build_timeline(
        [[interval("v", 0, 1000)], [interval("a", 0, 1000, track="mic")]]
    )
    assert len(timeline.intervals) == 2


def test_empty_interval_is_rejected() -> None:
    with pytest.raises(PlanError):
        interval("bad", 100, 100)
    with pytest.raises(PlanError):
        interval("bad", 200, 100)


def test_duplicate_interval_ids_are_rejected() -> None:
    timeline = Timeline()
    timeline.add(interval("dup", 0, 100))
    with pytest.raises(PlanError):
        timeline.add(interval("dup", 100, 200))


def test_gaps_between_intervals() -> None:
    gaps = gaps_between([interval("a", 0, 1000), interval("b", 1200, 2000)], "cam")
    assert len(gaps) == 1
    assert gaps[0].start_ticks == 1000
    assert gaps[0].end_ticks == 1200
    assert gaps[0].track == "cam"


def test_dependency_serialisation() -> None:
    timeline = build_timeline(
        [[interval("a", 0, 10)], [interval("b", 10, 20, depends_on=("a",))]]
    )
    payload = timeline.to_dict()
    assert payload["order"] == ["a", "b"]
    assert payload["dependencies"] == [{"dependent": "b", "prerequisite": "a"}]


def test_interval_limit_is_bounded() -> None:
    from mediasync import timeline as timeline_module

    original = timeline_module.MAX_INTERVALS
    timeline_module.MAX_INTERVALS = 2
    try:
        timeline = Timeline()
        timeline.add(interval("a", 0, 10))
        timeline.add(interval("b", 10, 20))
        with pytest.raises(PlanError):
            timeline.add(interval("c", 20, 30))
    finally:
        timeline_module.MAX_INTERVALS = original
