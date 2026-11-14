from fractions import Fraction

import pytest

from tlcore.conflicts import (
    LINK_MISALIGNED,
    OVERLAP,
    SOURCE_OVERRUN,
    TRANSITION_INVALID,
    ZERO_LENGTH,
    by_kind,
    scan_conflicts,
)
from tlcore.fixtures import baseline, build_timeline
from tlcore.model import Clip, LinkGroup, Timeline, Track, Transition
from tlcore.resolve import NonConvergenceError, resolve

S = 1_000_000_000


def _timeline(*tracks: Track, links: list[LinkGroup] | None = None,
              transitions: list[Transition] | None = None,
              declared: int = 5 * S) -> Timeline:
    return Timeline(
        fps=Fraction(25, 1), declared_duration_ns=declared,
        tracks=list(tracks), link_groups=links or [], transitions=transitions or [],
    )


def test_fixture_has_all_five_conflict_kinds():
    conflicts = scan_conflicts(build_timeline())
    counts = by_kind(conflicts)
    expected = baseline()["by_kind"]
    assert counts == expected
    assert len(conflicts) == baseline()["conflict_count"]
    assert set(counts) == {
        OVERLAP, ZERO_LENGTH, SOURCE_OVERRUN, LINK_MISALIGNED, TRANSITION_INVALID
    }
    overlap = next(item for item in conflicts if item.kind == OVERLAP)
    assert overlap.a_id and overlap.b_id and overlap.overlap_ns > 0


def test_half_open_adjacency_and_cross_track_are_not_conflicts():
    video = Track("video.main", "video", [
        Clip("a", "video.main", "video", 0, S),
        Clip("b", "video.main", "video", S, S),
    ])
    audio = Track("audio.vo", "audio", [Clip("vo", "audio.vo", "audio", 0, 2 * S)])
    assert scan_conflicts(_timeline(video, audio)) == []


def test_link_group_start_alignment_is_cross_track():
    video = Track("video.main", "video", [Clip("v", "video.main", "video", 0, 2 * S)])
    audio = Track("audio.vo", "audio", [Clip("a", "audio.vo", "audio", S, 2 * S)])
    conflicts = scan_conflicts(_timeline(
        video, audio, links=[LinkGroup("link.g", ["v", "a"])]
    ))
    assert len(conflicts) == 1
    assert conflicts[0].kind == LINK_MISALIGNED
    assert conflicts[0].overlap_ns == S
    assert {conflicts[0].a_id, conflicts[0].b_id} == {"v", "a"}


def test_valid_transition_overlap_is_allowed_but_over_limit_is_not():
    allowed = _timeline(
        Track("video.main", "video", [
            Clip("a", "video.main", "video", 0, 2 * S),
            Clip("b", "video.main", "video", S, 2 * S),
        ]),
        transitions=[Transition("tr.ok", "video.main", "a", "b", S, 2 * S)],
    )
    assert scan_conflicts(allowed) == []
    too_long = _timeline(
        Track("video.main", "video", [
            Clip("a", "video.main", "video", 0, 2 * S),
            Clip("b", "video.main", "video", S, 2 * S),
        ]),
        transitions=[Transition("tr.bad", "video.main", "a", "b", 2 * S, S)],
    )
    conflicts = scan_conflicts(too_long)
    assert [item.kind for item in conflicts] == [TRANSITION_INVALID]


def test_ripple_moves_downstream_to_fixed_point():
    track = Track("video.main", "video", [
        Clip("a", "video.main", "video", 0, 2 * S),
        Clip("b", "video.main", "video", S, 2 * S),
        Clip("c", "video.main", "video", 3 * S, S),
    ])
    result = resolve(_timeline(track), "ripple")
    assert result.fixed_point and result.unresolved == []
    assert result.timeline.clip_by_id()["b"].start_ns == 2 * S
    assert result.timeline.clip_by_id()["c"].start_ns == 4 * S


def test_ripple_moves_whole_link_group():
    video = Track("video.main", "video", [
        Clip("a", "video.main", "video", 0, 2 * S),
        Clip("b", "video.main", "video", S, 2 * S),
    ])
    audio = Track("audio.vo", "audio", [Clip("y", "audio.vo", "audio", S, 2 * S)])
    timeline = _timeline(video, audio, links=[LinkGroup("link.g", ["b", "y"])])
    result = resolve(timeline, "ripple")
    assert result.unresolved == []
    assert result.timeline.clip_by_id()["b"].start_ns == 2 * S
    assert result.timeline.clip_by_id()["y"].start_ns == 2 * S


def test_trim_respects_source_remaining_and_linked_peers():
    video = Track("video.main", "video", [
        Clip("a", "video.main", "video", 0, 2 * S),
        Clip("b", "video.main", "video", S, 2 * S, source_duration_ns=3 * S),
    ])
    audio = Track("audio.vo", "audio", [
        Clip("y", "audio.vo", "audio", S, 2 * S, source_duration_ns=3 * S),
    ])
    timeline = _timeline(video, audio, links=[LinkGroup("link.g", ["b", "y"])])
    result = resolve(timeline, "trim")
    assert result.unresolved == []
    assert result.timeline.clip_by_id()["b"].duration_ns == 2 * S
    assert result.timeline.clip_by_id()["y"].duration_ns == 2 * S


def test_trim_without_source_room_leaves_residual_conflict():
    video = Track("video.main", "video", [
        Clip("a", "video.main", "video", 0, 2 * S, source_duration_ns=2 * S),
        Clip("b", "video.main", "video", S, 2 * S),
    ])
    result = resolve(_timeline(video), "trim")
    assert result.fixed_point is False
    assert any(item.kind == OVERLAP for item in result.unresolved)


def test_drop_uses_priority_then_stable_clip_id():
    video = Track("video.main", "video", [
        Clip("z", "video.main", "video", 0, 2 * S, priority=10),
        Clip("a", "video.main", "video", S, 2 * S, priority=0),
    ])
    result = resolve(_timeline(video), "drop")
    assert result.unresolved == []
    assert "z" in result.timeline.clip_by_id()
    assert "a" not in result.timeline.clip_by_id()

    tied = Track("video.main", "video", [
        Clip("z", "video.main", "video", 0, 2 * S),
        Clip("a", "video.main", "video", S, 2 * S),
    ])
    dropped = resolve(_timeline(tied), "drop")
    assert "a" not in dropped.timeline.clip_by_id()


def test_iteration_limit_reports_nonconvergence():
    with pytest.raises(NonConvergenceError):
        resolve(build_timeline(), "ripple", max_iterations=1)
