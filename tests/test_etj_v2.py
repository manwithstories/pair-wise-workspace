from tlcore.etj_v2 import timeline_from_v2, timeline_to_v2
from tlcore.model import Segment, Timeline, Track


def test_v2_inheritance_and_relative_offsets():
    data = {
        "format": "etj",
        "version": 2,
        "name": "demo",
        "fps": "30000/1001",
        "timebase": 48000,
        "defaults": {"priority": 3, "metadata": {"studio": "internal"}},
        "tracks": [
            {
                "id": "v1",
                "kind": "video",
                "defaults": {"priority": 8, "locked": True, "extensions": {"x": 1}},
                "origin": {"samples": 48},
                "segments": [
                    {"id": "a", "offset": {"samples": 0}, "duration": {"samples": 96}},
                    {"id": "b", "offset": {"samples": 240}, "duration": {"samples": 48}, "priority": 20},
                ],
            }
        ],
    }
    timeline = timeline_from_v2(data)
    first, second = timeline.tracks[0].segments
    assert (first.start, second.start) == (48, 384)
    assert first.priority == 8 and second.priority == 20
    assert first.locked and first.extensions["x"] == 1
    assert first.metadata["studio"] == "internal"


def test_v2_roundtrip_preserves_samples_and_extension():
    timeline = Timeline(
        name="roundtrip",
        tracks=[
            Track(
                "v1",
                "video",
                segments=[Segment("a", 1, 17, source_start=3, extensions={"vendor": "ok"})],
            )
        ],
        declared_duration=19,
        extensions={"x_project": {"id": 7}},
    )
    data = timeline_to_v2(timeline)
    reread = timeline_from_v2(data)
    assert reread.track("v1").segments[0].start == 1
    assert reread.track("v1").segments[0].duration == 17
    assert reread.track("v1").segments[0].extensions["vendor"] == "ok"
    assert reread.extensions["x_project"] == {"id": 7}
