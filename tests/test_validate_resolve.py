from __future__ import annotations

import pytest

from cutplan.errors import CutplanError
from cutplan.model import Clip, Manifest, Timeline, Track
from cutplan.resolve import resolve_timeline, build_report, ResolutionStats
from cutplan.timebase import parse_rate
from cutplan.validate import beat_violations, closeup_violations, validate_timeline


def timeline_with(track: Track, *, duration: int = 300) -> Timeline:
    return Timeline(parse_rate("25"), duration, [track], Manifest("test"))


def test_validation_counts_shapes_and_zero_anomaly() -> None:
    track = Track(
        "overlay",
        "overlay",
        [
            Clip("nested-a", 0, 20),
            Clip("nested-b", 5, 10),
            Clip("chain-a", 40, 20),
            Clip("chain-b", 45, 10),
            Clip("chain-c", 50, 20),
            Clip("same-a", 100, 20),
            Clip("same-b", 100, 20),
            Clip("frame-a", 150, 20),
            Clip("frame-b", 169, 11),
            Clip("zero", 200, 0),
        ],
    )
    report = validate_timeline(timeline_with(track))
    assert report.tracks == 1
    assert report.clips == 10
    assert report.conflicts["clusters"] == 4
    assert report.conflicts["pairs"] == 6
    assert len(report.zero_duration) == 1
    codes = {error.code for error in report.errors}
    assert {"E_OVERLAP", "E_ZERO_DURATION"} <= codes


def test_cross_track_detection_and_snap_tolerance() -> None:
    main = Track("main", "main", [Clip("main-a", 0, 100)])
    closeup = Track("closeup", "closeup", [Clip("cu-a", 90, 20)])
    beat = Track("beat", "beat", [Clip("beat-near", 26, 5), Clip("beat-far", 32, 5)])
    timeline = Timeline(parse_rate("25"), 120, [main, closeup, beat], Manifest("x", ext={"beat_bpm": 120}))
    assert closeup_violations(timeline)[0]["clip_id"] == "cu-a"
    violations = beat_violations(timeline)
    assert [item["clip_id"] for item in violations] == ["beat-far"]


@pytest.mark.parametrize("policy", ["trim_head", "trim_tail", "ripple", "reject"])
def test_each_policy_resolves_and_is_idempotent(policy: str) -> None:
    track = Track(
        "t",
        "overlay",
        [Clip("a", 0, 20), Clip("b", 5, 15), Clip("c", 10, 25)],
    )
    first = resolve_timeline(timeline_with(track), policy)
    assert first.report["clusters_after"] == 0
    assert validate_timeline(first.timeline, include_overlap_errors=False).conflicts["clusters"] == 0
    second = resolve_timeline(first.timeline, policy)
    assert first.timeline.canonical_dict() == second.timeline.canonical_dict()


def test_trim_head_priority_and_zero_removal() -> None:
    track = Track(
        "t",
        "overlay",
        [Clip("same-high", 20, 30, priority=9), Clip("same-low", 20, 30, priority=0)],
    )
    result = resolve_timeline(timeline_with(track), "trim_head")
    assert [clip.id for clip in result.timeline.track("t").clips] == ["same-high"]
    assert result.report["removed_zero"] == 1


def test_ripple_shifts_following_clips_and_recomputes_duration() -> None:
    track = Track(
        "t",
        "overlay",
        [
            Clip("winner", 10, 20, priority=9),
            Clip("loser", 20, 20, priority=5),
            Clip("later", 40, 20, priority=0),
        ],
    )
    result = resolve_timeline(timeline_with(track, duration=60), "ripple")
    clips = {clip.id: clip for clip in result.timeline.track("t").clips}
    assert clips["winner"].start == 10
    assert clips["loser"].start == 30
    assert clips["later"].start == 50
    assert result.report["ripple_shifted"] == 2
    assert result.timeline.duration_frames == 70


def test_cross_track_orphan_and_isolation() -> None:
    main = Track("main", "main", [Clip("m", 0, 100)])
    closeup = Track("closeup", "closeup", [Clip("inside", 10, 20), Clip("outside", 90, 20)])
    timeline = Timeline(
        parse_rate("25"),
        120,
        [main, closeup],
        Manifest("x", ext={"cross_track": {"action": "orphan"}}),
    )
    result = resolve_timeline(timeline, "trim_head")
    assert [clip.id for clip in result.timeline.track("closeup").clips] == ["inside"]
    assert result.report["orphaned"] == 1
    assert result.report["orphans"][0]["reason"] == "closeup_outside_main"


def test_closeup_truncate_and_beat_snap() -> None:
    main = Track("main", "main", [Clip("m", 0, 120)])
    closeup = Track("closeup", "closeup", [Clip("cu", 100, 30)])
    beat = Track("beat", "beat", [Clip("b", 23, 5)])
    timeline = Timeline(
        parse_rate("25"),
        150,
        [main, closeup, beat],
        Manifest("x", ext={"cross_track": {"action": "truncate"}}),
    )
    result = resolve_timeline(timeline, "trim_head")
    close = result.timeline.track("closeup").clips[0]
    assert (close.start, close.duration) == (100, 20)
    hint = result.timeline.track("beat").clips[0]
    assert hint.start == 25


def test_unknown_policy_and_unknown_cross_action() -> None:
    timeline = timeline_with(Track("t", "overlay", [Clip("a", 0, 10)]))
    with pytest.raises(CutplanError, match="policy"):
        resolve_timeline(timeline, "drop")
    with pytest.raises(CutplanError, match="cross-track"):
        resolve_timeline(timeline, "trim_head", cross_track="explode")


def test_report_has_coverage_and_delta(tmp_path) -> None:
    track = Track("t", "main", [Clip("a", 0, 40), Clip("b", 50, 40)])
    timeline = timeline_with(track, duration=100)
    result = resolve_timeline(timeline, "trim_head")
    report = build_report(result.timeline, ResolutionStats(policy="trim_head"))
    assert report["declared_duration_frames"] == 100
    assert report["duration_frames"] == 90
    assert report["output_duration_frames"] == 100
    assert report["duration_delta_frames"] == -10
    assert report["tracks"][0]["coverage_frames"] == 80
    assert report["resolved_intervals"] == [
        {"track_id": "t", "start": 0, "end": 40},
        {"track_id": "t", "start": 50, "end": 90},
    ]
