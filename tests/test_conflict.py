import time

from tlcore.conflict import scan_conflicts
from tlcore.model import Segment, Timeline, Track


def timeline_with(tracks):
    return Timeline(name="conflict", tracks=tracks, declared_duration=200)


def test_adjacency_is_not_conflict_and_locked_is_flagged():
    locked = Segment("a", 0, 100, locked=True)
    timeline = timeline_with(
        [Track("v1", "video", segments=[locked, Segment("b", 100, 100)])]
    )
    assert scan_conflicts(timeline).conflict_count == 0
    locked.duration = 150
    report = scan_conflicts(timeline)
    assert report.conflict_count == 1
    assert report.conflicts[0].unresolvable
    assert "locked" in report.conflicts[0].reason


def test_invalid_intervals_are_classified():
    timeline = timeline_with(
        [Track("a1", "audio", segments=[Segment("zero", 0, 0), Segment("neg", 10, -2)])]
    )
    kinds = {item.kind for item in scan_conflicts(timeline).conflicts}
    assert {"zero_length", "negative_length"} <= kinds
    bad = Segment("bad", -1, 10)
    timeline = timeline_with([Track("v1", "video", segments=[bad])])
    assert any(item.kind == "out_of_bounds" for item in scan_conflicts(timeline).conflicts)


def test_occlusion_and_audio_peak_are_reported_without_conflicts():
    tracks = [
        Track("v1", "video", z_order=0, segments=[Segment("low", 0, 100)]),
        Track("v2", "video", z_order=1, segments=[Segment("high", 20, 30)]),
        Track("a1", "audio", segments=[Segment("a", 0, 50), Segment("b", 20, 20)]),
    ]
    report = scan_conflicts(timeline_with(tracks))
    assert report.conflict_count == 0
    assert report.occlusions[0]["lower_segment"] == "low"
    assert report.audio_peak == 2


def test_fifty_thousand_segment_scan_is_fast():
    segments = [Segment(f"s{i}", i * 100, 100) for i in range(50_000)]
    timeline = Timeline(tracks=[Track("v1", "video", segments=segments)])
    started = time.perf_counter()
    report = scan_conflicts(timeline, include_occlusions=False)
    elapsed = time.perf_counter() - started
    assert report.conflict_count == 0
    assert elapsed < 3.0
