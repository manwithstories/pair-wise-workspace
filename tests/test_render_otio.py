from fractions import Fraction

import opentimelineio as otio

from tlcore.fixtures import build_timeline
from tlcore.model import Clip, Timeline, Track
from tlcore.otio_bridge import (
    duration_report,
    from_otio,
    otio_duration_ns,
    read_otio,
    to_otio,
    write_otio,
)
from tlcore.render import build_frame_map, hash_frame_map, render_timeline

S = 1_000_000_000


def test_frame_count_half_open_and_local_frames():
    timeline = Timeline(
        fps=Fraction(25, 1), declared_duration_ns=S,
        tracks=[Track("video.main", "video", [
            Clip("a", "video.main", "video", 0, S, source_in_ns=S),
        ])],
    )
    frame_map = build_frame_map(timeline, 25)
    assert len(frame_map) == 25
    assert frame_map[0][0].local_frame == 0
    assert frame_map[0][0].source_frame == 25
    assert frame_map[-1][0].local_frame == 24
    assert all(len(entries) == 1 for entries in frame_map)


def test_frame_start_rounds_up_to_next_frame():
    timeline = Timeline(
        fps=Fraction(25, 1), declared_duration_ns=2 * S,
        tracks=[Track("video.main", "video", [
            Clip("a", "video.main", "video", S // 5, 2 * S // 5),
        ])],
    )
    frame_map = build_frame_map(timeline, 25)
    assert frame_map[5][0].clip_id == "a"
    assert frame_map[5][0].local_frame == 0
    assert frame_map[4] == []


def test_layer_order_and_max_layers():
    timeline = Timeline(
        fps=Fraction(25, 1), declared_duration_ns=2 * S,
        tracks=[
            Track("video.main", "video", [
                Clip("a", "video.main", "video", 0, 2 * S),
                Clip("b", "video.main", "video", S, S),
            ]),
            Track("audio.vo", "audio", [Clip("vo", "audio.vo", "audio", 0, 2 * S)]),
        ],
    )
    result = render_timeline(timeline, 25)
    assert result.frames == 50
    assert result.layers_max == 3
    entries = result.frame_map[25]
    assert [entry.clip_id for entry in entries] == ["a", "b", "vo"]
    assert [entry.layer for entry in entries] == [0, 1, 2]


def test_frame_hash_is_stable_and_64_hex():
    timeline = Timeline(
        declared_duration_ns=S,
        tracks=[Track("video.main", "video", [Clip("a", "video.main", "video", 0, S)])],
    )
    first = hash_frame_map(build_frame_map(timeline, 25))
    second = hash_frame_map(build_frame_map(timeline, 25))
    assert first == second
    assert len(first) == 64
    assert int(first, 16) >= 0


def test_otio_round_trip_preserves_exact_model():
    original = build_timeline()
    otio_timeline = to_otio(original)
    restored = from_otio(otio_timeline)
    assert restored.declared_duration_ns == original.declared_duration_ns
    assert restored.computed_duration_ns == original.computed_duration_ns
    assert len(restored.clip_by_id()) == len(original.clip_by_id())
    for clip_id, clip in original.clip_by_id().items():
        peer = restored.clip_by_id()[clip_id]
        assert (peer.start_ns, peer.duration_ns, peer.source_in_ns) == (
            clip.start_ns, clip.duration_ns, clip.source_in_ns
        )
    assert len(restored.link_groups) == len(original.link_groups)
    assert len(restored.transitions) == len(original.transitions)
    assert len(restored.markers) == len(original.markers)


def test_otio_duration_deviation_is_zero(tmp_path):
    timeline = build_timeline()
    path = tmp_path / "show.otio"
    write_otio(timeline, path)
    loaded = read_otio(path)
    assert otio_duration_ns(otio.adapters.read_from_file(str(path))) == 60 * S
    report = duration_report(timeline, loaded)
    assert report["deviation_ns"] == 0
    assert report["otio_duration_ns"] == 60 * S


def test_generic_otio_without_metadata_is_read_sequentially():
    track = otio.schema.Track(name="video.main", kind="Video")
    first = otio.schema.Clip(
        name="a",
        source_range=otio.opentime.TimeRange(
            start_time=otio.opentime.RationalTime(0, S),
            duration=otio.opentime.RationalTime(2 * S, S),
        ),
    )
    second = otio.schema.Clip(
        name="b",
        source_range=otio.opentime.TimeRange(
            start_time=otio.opentime.RationalTime(3 * S, S),
            duration=otio.opentime.RationalTime(S, S),
        ),
    )
    track.append(first)
    track.append(second)
    otio_timeline = otio.schema.Timeline()
    otio_timeline.tracks.append(track)
    timeline = from_otio(otio_timeline)
    clips = timeline.track_by_id()["video.main"].clips
    assert [clip.id for clip in clips] == ["a", "b"]
    assert [clip.start_ns for clip in clips] == [0, 2 * S]
    assert timeline.declared_duration_ns == 3 * S
