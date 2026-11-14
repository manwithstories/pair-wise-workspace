from __future__ import annotations

import json

import pytest

from cutplan.convert import convert_file, detect_version_text, load_timeline, parse_text, save_timeline
from cutplan.errors import CutplanError
from cutplan.fmt_v1 import dumps_v1, parse_v1
from cutplan.fmt_v2 import dumps_v2, parse_v2
from cutplan.model import Clip, Manifest, Timeline, Track
from cutplan.resolve import resolve_timeline
from cutplan.timebase import parse_rate


def sample_timeline() -> Timeline:
    return Timeline(
        rate=parse_rate("30000/1001"),
        declared_duration=120,
        tracks=[
            Track(
                "main",
                "main",
                [Clip("m1", 0, 20, ext={"pipe|key": "pipe|value", "nested": {"x": [1, 2]}})],
                priority=1,
                ext={"track_ext": True},
                metadata={"note": "track"},
            ),
            Track("audio", "audio", [Clip("a1", 10, 5)], priority=2),
        ],
        manifest=Manifest(
            "A | B",
            ext={"cross_track": {"action": "orphan"}, "pipe": "x|y"},
            metadata={"owner": "ops"},
        ),
        ext={"top": {"a": 1}, "beat_bpm": 120},
        metadata={"resolution_policy": "trim_head"},
    )


def test_v1_roundtrip_preserves_ext_and_pipe_fields() -> None:
    timeline = sample_timeline()
    text = dumps_v1(timeline)
    assert text.startswith("CUTPLAN|1\n")
    assert detect_version_text(text) == "v1"
    assert parse_v1(text).canonical_dict() == timeline.canonical_dict()


def test_v2_roundtrip_and_schema() -> None:
    timeline = sample_timeline()
    text = dumps_v2(timeline)
    root = json.loads(text)
    assert root["schema"] == "cutplan/v2"
    assert root["version"] == 2
    assert detect_version_text(text) == "v2"
    assert parse_v2(text).canonical_dict() == timeline.canonical_dict()


def test_audio_half_frame_rounds_half_up_v1() -> None:
    text = dumps_v1(sample_timeline()).replace('CLIP|"audio"|"a1"|10|', 'CLIP|"audio"|"a1"|10.5|')
    timeline = parse_v1(text)
    assert timeline.track("audio").clips[0].start == 11
    result = resolve_timeline(timeline, "trim_head")
    assert result.report["audio_start_rounded"] == 1


def test_audio_half_frame_rounds_half_up_v2() -> None:
    root = json.loads(dumps_v2(sample_timeline()))
    for track in root["tracks"]:
        if track["id"] == "audio":
            track["clips"][0]["start"] = 10.5
    timeline = parse_v2(json.dumps(root))
    assert timeline.track("audio").clips[0].start == 11


def test_fractional_frame_rejected_outside_audio() -> None:
    text = dumps_v1(sample_timeline()).replace('CLIP|"main"|"m1"|0|', 'CLIP|"main"|"m1"|0.5|')
    with pytest.raises(CutplanError, match="audio|integer frame"):
        parse_v1(text)
    root = json.loads(dumps_v2(sample_timeline()))
    root["tracks"][0]["clips"][0]["start"] = 0.5
    with pytest.raises(CutplanError, match="integer frame"):
        parse_v2(json.dumps(root))


def test_unknown_version_lists_supported() -> None:
    with pytest.raises(CutplanError, match="supported: v1, v2"):
        detect_version_text("CUTPLAN|3\nEND\n")
    with pytest.raises(CutplanError, match="supported: v1, v2"):
        detect_version_text('{"schema":"cutplan/v9","version":9}')


def test_v2_schema_rejects_unknown_and_bad_fields() -> None:
    root = json.loads(dumps_v2(sample_timeline()))
    root["unexpected"] = True
    with pytest.raises(CutplanError, match="unknown field"):
        parse_v2(json.dumps(root))
    root = json.loads(dumps_v2(sample_timeline()))
    root["tracks"][0]["clips"][0]["duration"] = 1.5
    with pytest.raises(CutplanError, match="duration"):
        parse_v2(json.dumps(root))


def test_conversion_preserves_ext(tmp_path) -> None:
    source = tmp_path / "source.txt"
    destination = tmp_path / "destination.json"
    save_timeline(sample_timeline(), source, "v1")
    timeline, target = convert_file(source, destination, "v2")
    assert target == "v2"
    loaded = load_timeline(destination)
    assert timeline.canonical_dict() == loaded.canonical_dict()
    assert loaded.manifest.ext["cross_track"]["action"] == "orphan"
    assert loaded.track("main").clips[0].ext["nested"] == {"x": [1, 2]}


def test_parse_text_and_unknown_format() -> None:
    timeline = parse_text(dumps_v2(sample_timeline()))
    assert timeline.clip_count == 2
    with pytest.raises(CutplanError, match="detect"):
        parse_text("just some text")
