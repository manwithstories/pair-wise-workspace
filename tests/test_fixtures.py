from tlcore.conflict import scan_conflicts
from tlcore.resolve import resolve
from tlcore.schedule import build_schedule
from tools.gen_fixtures import build_episode


def test_fixture_generator_has_acceptance_scale():
    timeline = build_episode()
    report = scan_conflicts(timeline)
    assert report.conflict_count == 48
    assert len(report.pairs) == 48
    assert len(timeline.tracks) == 7
    assert timeline.segment_count() == 170
    resolved = resolve(timeline, ["ripple", "trim"])
    assert resolved.resolved_intervals == 170
    assert resolved.unresolved == []
    assert resolved.drift_samples <= 100
    assert scan_conflicts(resolved.timeline).conflict_count == 0
    schedule = build_schedule(resolved.timeline)
    assert schedule.schedule_frames == 20_000
    assert len(schedule.digest) == 64
