from fractions import Fraction

from tlcore.model import Segment, Timeline, Track, validate_model


def make_timeline() -> Timeline:
    return Timeline(
        name="test",
        fps=Fraction(24),
        tracks=[
            Track(
                id="v1",
                kind="video",
                segments=[Segment("a", 0, 100), Segment("b", 100, 50)],
            )
        ],
    )


def test_duration_and_half_open_boundary():
    timeline = make_timeline()
    assert timeline.duration() == 150
    assert timeline.segment_count() == 2


def test_effective_lock_and_copy():
    timeline = make_timeline()
    timeline.tracks[0].locked = True
    assert timeline.effective_locked(timeline.tracks[0], timeline.tracks[0].segments[0])
    clone = timeline.copy()
    clone.tracks[0].segments[0].start = 10
    assert timeline.tracks[0].segments[0].start == 0


def test_validation_reports_duplicates():
    timeline = make_timeline()
    timeline.tracks.append(
        Track(id="v1", kind="audio", segments=[Segment("b", 0, 1)])
    )
    errors = validate_model(timeline)
    assert any("duplicate track" in error for error in errors)
    assert any("duplicate segment" in error for error in errors)
