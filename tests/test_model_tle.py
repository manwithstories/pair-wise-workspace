import json
from fractions import Fraction

import pytest

from tlcore.fixtures import build_timeline
from tlcore.model import Clip, ModelError, Timeline, Track
from tlcore.tle import (
    TLEError,
    downgrade_v2_document,
    parse_timeline_document,
    read_tle,
    to_v1_document,
    to_v2_document,
    upgrade_v1_document,
    write_tle,
)


def test_fixture_model_is_valid_and_has_four_tracks():
    timeline = build_timeline()
    timeline.validate()
    assert [track.id for track in timeline.tracks] == [
        "video.main", "audio.vo", "subtitle.main", "audio.music"
    ]
    assert timeline.computed_duration_ns == 60_000_000_000
    assert len(timeline.link_groups) == 2
    assert len(timeline.transitions) == 3


def test_v2_document_round_trip(tmp_path):
    timeline = build_timeline()
    path = tmp_path / "show_v2.json"
    warnings = write_tle(timeline, path)
    assert warnings == []
    loaded = read_tle(path)
    assert loaded.version == 2
    assert loaded.fps == Fraction(25, 1)
    assert loaded.declared_duration_ns == timeline.declared_duration_ns
    assert len(loaded.clip_by_id()) == len(timeline.clip_by_id())
    original = timeline.clip_by_id()["v.scene2"]
    restored = loaded.clip_by_id()["v.scene2"]
    assert (restored.start_ns, restored.duration_ns, restored.source_in_ns) == (
        original.start_ns, original.duration_ns, original.source_in_ns
    )
    assert restored.metadata == original.metadata


def test_v1_round_trip_and_downgrade_warnings(tmp_path):
    timeline = build_timeline()
    document, warnings = to_v1_document(timeline)
    assert any("link" in warning for warning in warnings)
    assert any("transition" in warning for warning in warnings)
    assert "links" not in document
    assert "transitions" not in document
    path = tmp_path / "show_v1.json"
    write_tle(timeline, path, version=1)
    loaded = read_tle(path)
    assert loaded.version == 1
    assert loaded.link_groups == []
    assert loaded.transitions == []
    assert len(loaded.clip_by_id()) == len(timeline.clip_by_id())
    assert loaded.clip_by_id()["v.overrun"].source_duration_ns == 10_000_000_000


def test_upgrade_and_downgrade_document_helpers():
    v1_document, _ = to_v1_document(build_timeline())
    upgraded = upgrade_v1_document(v1_document)
    assert upgraded["version"] == 2
    assert upgraded["links"] == []
    v2_document = to_v2_document(build_timeline())
    downgraded, warnings = downgrade_v2_document(v2_document)
    assert downgraded["version"] == 1
    assert len(warnings) >= 2


def _doc(clip: dict) -> dict:
    return {
        "version": 2,
        "fps": 25,
        "duration": {"num": 10, "den": 1},
        "tracks": [{"id": "video.main", "kind": "video", "clips": [clip]}],
    }


def test_zero_denominator_reports_source_and_clip_id():
    document = _doc({
        "id": "clip.bad",
        "start": {"num": 1, "den": 0},
        "duration": {"num": 1, "den": 1},
    })
    with pytest.raises(TLEError) as caught:
        parse_timeline_document(document, source="show.json")
    assert caught.value.source == "show.json"
    assert caught.value.clip_id == "clip.bad"
    assert "denominator" in str(caught.value)


def test_negative_duration_and_missing_field_are_located():
    negative = _doc({
        "id": "clip.negative",
        "start": {"num": 0, "den": 1},
        "duration": {"num": -1, "den": 1},
    })
    with pytest.raises(TLEError) as caught:
        parse_timeline_document(negative, source="negative.json")
    assert caught.value.clip_id == "clip.negative"
    assert "negative" in str(caught.value)

    missing = _doc({"id": "clip.missing", "start": {"num": 0, "den": 1}})
    with pytest.raises(TLEError) as caught:
        parse_timeline_document(missing, source="missing.json")
    assert caught.value.clip_id == "clip.missing"
    assert "missing required duration" in str(caught.value)


def test_duplicate_clip_id_is_rejected_by_model():
    timeline = Timeline(
        declared_duration_ns=10,
        tracks=[Track("video.main", "video", [
            Clip("same", "video.main", "video", 0, 1),
            Clip("same", "video.main", "video", 1, 1),
        ])],
    )
    with pytest.raises(ModelError, match="duplicate clip id"):
        timeline.validate()


def test_v1_in_out_aliases_and_track_markers_are_accepted():
    document = {
        "version": 1,
        "duration_ns": 5_000_000_000,
        "tracks": [{
            "id": "video.main",
            "kind": "video",
            "clips": [{
                "id": "a", "in_ns": 0, "out_ns": 2_000_000_000,
                "source_in_ns": 1_000_000_000, "source_length_ns": 4_000_000_000,
            }],
            "markers": [{"id": "mark", "at": 1_000_000_000, "name": "beat"}],
        }],
    }
    parsed = parse_timeline_document(document, source="aliases.json")
    clip = parsed.clip_by_id()["a"]
    assert (clip.start_ns, clip.duration_ns, clip.source_in_ns) == (
        0, 2_000_000_000, 1_000_000_000
    )
    assert clip.source_duration_ns == 4_000_000_000
    assert parsed.markers[0].track == "video.main"


def test_invalid_json_has_file_location(tmp_path):
    path = tmp_path / "broken.json"
    path.write_text('{"version": 2,', encoding="utf-8")
    with pytest.raises(TLEError) as caught:
        read_tle(path)
    assert str(path) in str(caught.value)
    assert "line 1" in str(caught.value)
