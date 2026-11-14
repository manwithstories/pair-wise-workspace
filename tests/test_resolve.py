from tlcore.conflict import scan_conflicts
from tlcore.model import Segment, Timeline, Track
from tlcore.resolve import resolve


def make_timeline(*segments: Segment, kind: str = "video") -> Timeline:
    return Timeline(tracks=[Track("v1", kind, segments=list(segments))])


def test_trim_removes_head_and_is_idempotent():
    timeline = make_timeline(Segment("a", 0, 100), Segment("b", 50, 100))
    result = resolve(timeline, ["trim"])
    second = result.timeline.track("v1").segments[1]
    assert second.start == 100 and second.duration == 50 and second.source_start == 50
    assert scan_conflicts(result.timeline).conflict_count == 0
    rerun = resolve(result.timeline, ["trim"])
    assert rerun.as_dict()["unresolved_count"] == 0
    assert scan_conflicts(rerun.timeline).conflict_count == 0


def test_ripple_extends_timeline_without_losing_duration():
    timeline = make_timeline(Segment("a", 0, 100), Segment("b", 50, 100))
    result = resolve(timeline, ["ripple"])
    assert [segment.start for segment in result.timeline.track("v1").segments] == [0, 100]
    assert result.after_samples == 200
    assert result.before_samples == 150
    assert result.drift_samples == 50


def test_drop_removes_low_priority_segment():
    timeline = make_timeline(
        Segment("keep", 0, 100, priority=10),
        Segment("drop", 20, 100, priority=1),
    )
    result = resolve(timeline, ["drop"])
    assert [segment.id for segment in result.timeline.track("v1").segments] == ["keep"]


def test_spill_creates_independent_track():
    timeline = make_timeline(Segment("a", 0, 100), Segment("b", 50, 100))
    result = resolve(timeline, ["spill"])
    assert len(result.timeline.tracks) == 2
    assert scan_conflicts(result.timeline).conflict_count == 0


def test_fill_covers_track_gap():
    timeline = make_timeline(Segment("a", 0, 100), Segment("b", 200, 100))
    result = resolve(timeline, ["fill"])
    fillers = [segment for segment in result.timeline.track("v1").segments if segment.id.startswith("fill.")]
    assert len(fillers) == 1
    assert (fillers[0].start, fillers[0].duration) == (100, 100)
    assert result.fillers_added == 1


def test_locked_segment_stays_unresolved():
    timeline = make_timeline(
        Segment("locked", 0, 100, locked=True), Segment("other", 50, 100)
    )
    result = resolve(timeline, ["trim", "ripple", "drop"])
    assert len(result.unresolved) == 1
    assert result.unresolved[0].kind == "unresolvable"
    assert "locked" in result.unresolved[0].reason
    assert result.timeline.track("v1").segments[1].start == 50
