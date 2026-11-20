"""Timeline validation and source-bound resolution."""

from __future__ import annotations

import pytest

from mediacore.errors import ParameterError, TimelineError
from mediacore.timeline import (
    ClipSpec,
    Timeline,
    TrackSpec,
    TransitionSpec,
)


def _track(track_id="v", kind="video", timebase="1/15360", **kw):
    return TrackSpec(id=track_id, path="x.mp4", kind=kind, timebase=timebase, **kw)


def test_resolve_pins_boundaries_to_source_ticks():
    timeline = Timeline()
    timeline.add_track(_track())
    timeline.add_clip(ClipSpec(id="c", track="v", start_us=1_000_000, end_us=2_000_000))
    resolved = timeline.resolve()[0]
    assert resolved.start_tick == 15360
    assert resolved.end_tick == 30720
    assert resolved.start_us == 1_000_000


def test_unknown_track_reference_is_rejected():
    timeline = Timeline()
    timeline.add_clip(ClipSpec(id="c", track="missing", start_us=0, end_us=1_000))
    with pytest.raises(TimelineError) as info:
        timeline.validate()
    assert info.value.code == "timeline.unknown_track"


def test_negative_clip_start_is_rejected():
    timeline = Timeline()
    timeline.add_track(_track())
    timeline.add_clip(ClipSpec(id="c", track="v", start_us=0, end_us=1_000))
    # Mutate after construction to bypass the constructor's own check.
    object.__setattr__(timeline.clips["c"], "start_us", -1)
    with pytest.raises(TimelineError) as info:
        timeline.validate()
    assert info.value.code == "timeline.out_of_range"


def test_transition_licenses_overlap_exactly():
    timeline = Timeline()
    timeline.add_track(_track())
    timeline.add_clip(ClipSpec(id="a", track="v", start_us=0, end_us=1_000_000))
    timeline.add_clip(ClipSpec(id="b", track="v", start_us=900_000, end_us=2_000_000))
    timeline.add_transition(TransitionSpec(id="x", left="a", right="b", duration_us=100_000))
    timeline.validate()  # must not raise
    assert timeline.transitions["x"].node_id == "transition:x"


def test_overlap_longer_than_transition_is_rejected():
    timeline = Timeline()
    timeline.add_track(_track())
    timeline.add_clip(ClipSpec(id="a", track="v", start_us=0, end_us=1_000_000))
    timeline.add_clip(ClipSpec(id="b", track="v", start_us=700_000, end_us=2_000_000))
    timeline.add_transition(TransitionSpec(id="x", left="a", right="b", duration_us=100_000))
    with pytest.raises(TimelineError) as info:
        timeline.validate()
    assert info.value.code == "timeline.overlap_unlicensed"


def test_transition_longer_than_clip_is_rejected():
    timeline = Timeline()
    timeline.add_track(_track())
    timeline.add_clip(ClipSpec(id="a", track="v", start_us=0, end_us=100_000))
    timeline.add_clip(ClipSpec(id="b", track="v", start_us=50_000, end_us=2_000_000))
    timeline.add_transition(TransitionSpec(id="x", left="a", right="b", duration_us=200_000))
    with pytest.raises(TimelineError) as info:
        timeline.validate()
    assert info.value.code == "timeline.transition_too_long"


def test_duplicate_ids_are_rejected():
    timeline = Timeline()
    timeline.add_track(_track())
    with pytest.raises(TimelineError) as info:
        timeline.add_track(_track())
    assert info.value.code == "timeline.duplicate_id"


def test_track_timebase_is_validated_eagerly():
    with pytest.raises(ParameterError):
        _track(timebase="bogus")


def test_roundtrip_json_and_fingerprint_stable():
    built = Timeline.from_dict(_dict_payload())
    assert built.fingerprint() == Timeline.from_dict(_dict_payload()).fingerprint()
    assert built.to_dict()["clips"][0]["id"] == "a"


def _dict_payload():
    return {
        "tracks": [{"id": "v", "path": "x", "kind": "video", "timebase": "1/15360"}],
        "clips": [{"id": "a", "track": "v", "start_us": 0, "end_us": 1_000_000}],
        "transitions": [],
    }
