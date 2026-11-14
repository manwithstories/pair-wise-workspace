from __future__ import annotations

import pytest

from cutplan.errors import CutplanError
from cutplan.interval import (
    adjacent,
    cluster_stats,
    contains,
    covered_frames,
    intersection,
    merge_intervals,
    overlap_pairs,
    overlaps,
    scan_clusters,
    subtract,
)
from cutplan.model import Clip, Manifest, Timeline, Track
from cutplan.timebase import FrameInterval, parse_rate


def spans(*values: tuple[int, int]) -> list[FrameInterval]:
    return [FrameInterval(start, end) for start, end in values]


def test_half_open_operations() -> None:
    left, right = FrameInterval(0, 10), FrameInterval(5, 15)
    assert overlaps(left, right)
    assert intersection(left, right) == FrameInterval(5, 10)
    assert contains(FrameInterval(0, 20), FrameInterval(5, 10))
    assert not contains(FrameInterval(0, 20), FrameInterval(15, 25))
    touching = FrameInterval(10, 20)
    assert adjacent(left, touching)
    assert not overlaps(left, touching)
    assert subtract(left, right) == [FrameInterval(0, 5)]
    assert subtract(left, FrameInterval(20, 30)) == [left]


def test_merge_and_coverage() -> None:
    assert merge_intervals(spans((0, 10), (10, 20), (25, 30))) == spans((0, 20), (25, 30))
    assert covered_frames(spans((0, 10), (5, 15), (10, 20))) == 20


def test_conflict_shapes() -> None:
    nested = spans((0, 100), (10, 20))
    chain = spans((0, 20), (5, 15), (10, 25))
    identical = spans((40, 60), (40, 60))
    single_frame = spans((100, 120), (119, 130))
    assert scan_clusters(nested) == [[0, 1]]
    assert scan_clusters(chain) == [[0, 1, 2]]
    assert overlap_pairs(nested) == [(0, 1)]
    assert overlap_pairs(chain) == [(0, 1), (0, 2), (1, 2)]
    assert scan_clusters(identical) == [[0, 1]]
    assert overlap_pairs(single_frame) == [(0, 1)]
    assert scan_clusters(spans((0, 10), (10, 20), (30, 30))) == []
    stats = cluster_stats(spans((0, 10), (5, 8), (5, 8), (30, 30)))
    assert stats["clusters"] == 1
    assert stats["pairs"] == 3


def test_model_invariants_and_canonical_order() -> None:
    timeline = Timeline(
        rate=parse_rate("25"),
        declared_duration=100,
        tracks=[
            Track("b", "overlay", [Clip("z", 10, 5), Clip("a", 0, 5)]),
            Track("a", "main", [Clip("m", 0, 20)]),
        ],
        manifest=Manifest("demo", ext={"x": 1}),
    )
    assert timeline.clip_count == 3
    assert timeline.duration_frames == 20
    assert timeline.output_duration_frames == 100
    canonical = timeline.canonical_dict()
    assert [track["id"] for track in canonical["tracks"]] == ["a", "b"]
    assert [clip["id"] for clip in canonical["tracks"][1]["clips"]] == ["a", "z"]
    with pytest.raises(CutplanError):
        Timeline(parse_rate("25"), 1, [Track("x", "main", [Clip("c", 0, 1)]), Track("x", "main")])
