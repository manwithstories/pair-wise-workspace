import json
from pathlib import Path

from tlcore.etj_v1 import read_v1, timeline_to_v1, write_v1
from tlcore.model import Segment, Timeline, Track


def test_v1_flat_roundtrip_and_unknown_extension(tmp_path: Path):
    source = {
        "format": "etj",
        "version": 1,
        "name": "demo",
        "fps": "25",
        "duration_ms": 4000,
        "tracks": [{"id": "v1", "kind": "video", "priority": 5, "x_vendor": "t1"}],
        "clips": [
            {
                "id": "a",
                "track": "v1",
                "start_ms": 0,
                "duration_ms": 4000,
                "priority": 10,
                "x_vendor": {"keep": True},
            }
        ],
    }
    path = tmp_path / "input.json"
    path.write_text(json.dumps(source), encoding="utf-8")
    timeline = read_v1(path)
    assert timeline.duration() == 192_000
    assert timeline.tracks[0].segments[0].priority == 10
    assert timeline.tracks[0].segments[0].extensions["x_vendor"] == {"keep": True}
    out = tmp_path / "out.json"
    write_v1(timeline, out)
    reread = json.loads(out.read_text(encoding="utf-8"))
    assert reread["clips"][0]["x_vendor"] == {"keep": True}
    assert reread["tracks"][0]["x_vendor"] == "t1"


def test_bankers_quantization_warns_each_clip_time(tmp_path: Path):
    timeline = Timeline(
        tracks=[
            Track(
                "v1",
                "video",
                segments=[Segment("a", 24, 72, source_start=24)],
            )
        ],
        declared_duration=96,
    )
    document, warnings = timeline_to_v1(timeline)
    clip = document["clips"][0]
    assert (clip["start_ms"], clip["duration_ms"], clip["source_in_ms"]) == (0, 2, 0)
    assert document["duration_ms"] == 2
    assert len(warnings) == 3
