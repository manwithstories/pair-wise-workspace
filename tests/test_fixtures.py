import pytest

from mkvtool.ebml.tree import parse_ebml
from mkvtool.errors import InputParseError
from mkvtool.fixtures import FixtureOptions, build_fixture
from mkvtool.mkv.demux import demux_bytes
from mkvtool.report.tree_summary import build_tree_summary


def test_unknown_segment_fixture_shape():
    fixture = build_fixture(FixtureOptions(
        "unknown.mkv", unknown_segment=True, unknown_clusters=8,
    ))
    summary = build_tree_summary(parse_ebml(fixture.data))
    assert 4100 <= summary["element_count"] <= 4400
    assert summary["unknown_size_elements"] == 9
    assert summary["max_depth"] == 6


def test_negative_b_fixture_has_1500_video_frames_and_38_keyframes():
    fixture = build_fixture(FixtureOptions(
        "negative.mkv", negative_dts=True, b_frames=True, unknown_clusters=4,
    ))
    demux = demux_bytes(fixture.data)
    assert len(demux.tracks) == 2
    assert len(demux.samples[1]) == 1500
    assert sum(1 for sample in demux.samples[1] if sample.keyframe) == 38
    assert demux.samples[1][0].timestamp_ticks == -80
    assert any(sample.composition_offset_ticks > 0 for sample in demux.samples[1])


def test_truncated_fixture_keeps_usable_prefix():
    fixture = build_fixture(FixtureOptions(
        "trunc.mkv", video_frames=300, audio_frames=300, cluster_count=3,
        unknown_segment=True, unknown_clusters=3, truncate=True,
    ))
    demux = demux_bytes(fixture.data)
    assert demux.parse.truncated
    assert len(demux.samples[1]) > 0
    assert len(demux.samples[1]) < 300


def test_corrupt_fixture_header_raises():
    fixture = build_fixture(FixtureOptions("bad.mkv", corrupt_header=True))
    with pytest.raises(InputParseError):
        demux_bytes(fixture.data)
