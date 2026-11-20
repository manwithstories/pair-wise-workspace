"""Ingest tests: three raw codecs, VFR indexes and rejection of bad indexes."""

from __future__ import annotations

import json
from fractions import Fraction
from pathlib import Path

import pytest

from mediasync.errors import CodecConflictError, IndexError_, InputError, StreamError
from mediasync.fixtures import FixtureConfig, build_video_fixture
from mediasync.ingest import Limits, load_audio_track, load_video_track, sniff_codec


@pytest.mark.parametrize("codec", ["h264", "hevc", "av1"])
def test_three_raw_codecs_are_ingested(
    tmp_path: Path, codec: str, request: pytest.FixtureRequest
) -> None:
    bundle = request.getfixturevalue(
        {"h264": "h264_bundle", "hevc": "hevc_bundle", "av1": "av1_bundle"}[codec]
    )
    video = bundle.load_video()
    assert video.codec == codec
    assert video.packets
    assert video.duration_ticks > 0
    assert video.keyframes
    assert all(packet.keyframe for packet in video.packets if packet.index == 0)
    assert sniff_codec(video.path) == codec


def test_av1_uses_obu_container(tmp_path: Path, av1_bundle) -> None:
    video = av1_bundle.load_video()
    assert video.path.suffix == ".obu"
    assert video.codec == "av1"
    payload = json.loads(video.path.with_suffix("").with_name("video0.index.json").read_text())
    assert payload["codec"] == "av1"


def test_variable_frame_rate_index(h264_bundle) -> None:
    from mediasync.fixtures import make_timestamps

    entries = make_timestamps(20, Fraction(30), vfr_every=4)
    durations = {entry["duration_ticks"] for entry in entries}
    assert len(durations) == 2, "VFR fixture should mix nominal and stretched frames"


def test_index_bounds_are_enforced(h264_bundle) -> None:
    def beyond(payload):
        payload["packets"][0]["offset"] = payload["stream_size"] + 10

    path = h264_bundle.clone_index("out-of-range.index.json", beyond)
    with pytest.raises(IndexError_):
        load_video_track(path, limits=Limits())


def test_negative_duration_is_rejected(h264_bundle) -> None:
    def negative(payload):
        payload["packets"][1]["duration_ticks"] = -1

    path = h264_bundle.clone_index("negative.index.json", negative)
    with pytest.raises(IndexError_):
        load_video_track(path, limits=Limits())


def test_unknown_codec_is_rejected(h264_bundle) -> None:
    def unknown(payload):
        payload["codec"] = "mpeg2video"

    path = h264_bundle.clone_index("unknown.index.json", unknown)
    with pytest.raises(CodecConflictError):
        load_video_track(path, limits=Limits())


def test_codec_mismatch_between_index_and_stream(h264_bundle) -> None:
    def mismatch(payload):
        payload["codec"] = "hevc"

    path = h264_bundle.clone_index("mismatch.index.json", mismatch)
    with pytest.raises(CodecConflictError):
        load_video_track(path, limits=Limits())


def test_overlapping_packets_are_rejected(h264_bundle) -> None:
    def overlap(payload):
        payload["packets"][2]["offset"] = payload["packets"][1]["offset"]

    path = h264_bundle.clone_index("overlap.index.json", overlap)
    with pytest.raises(IndexError_):
        load_video_track(path, limits=Limits())


def test_index_requires_keyframe(h264_bundle) -> None:
    def strip_keyframes(payload):
        for packet in payload["packets"]:
            packet["keyframe"] = False

    path = h264_bundle.clone_index("no-keyframe.index.json", strip_keyframes)
    with pytest.raises(IndexError_):
        load_video_track(path, limits=Limits())


def test_index_limits(h264_bundle) -> None:
    with pytest.raises(InputError):
        load_video_track(
            h264_bundle.fixture.index_path,
            limits=Limits(max_stream_bytes=100),
        )
    with pytest.raises(InputError):
        load_video_track(
            h264_bundle.fixture.index_path,
            limits=Limits(max_index_bytes=10),
        )


def test_truncated_stream_is_rejected_strictly(h264_bundle) -> None:
    path, truncated = h264_bundle.truncated_index(
        "truncated.index.json", h264_bundle.fixture.size_bytes // 2
    )
    with pytest.raises(IndexError_):
        load_video_track(path, limits=Limits())


def test_truncated_stream_marks_boundary_when_allowed(h264_bundle) -> None:
    path, truncated = h264_bundle.truncated_index(
        "truncated-allow.index.json", h264_bundle.fixture.size_bytes // 2
    )
    video = load_video_track(path, limits=Limits(), allow_truncated=True)
    assert video.truncated_indices, "truncation must be recorded, not hidden"


def test_empty_index_file(tmp_path: Path) -> None:
    path = tmp_path / "empty.index.json"
    path.write_text("")
    with pytest.raises(InputError):
        load_video_track(path, limits=Limits())


def test_unexplained_stream_is_rejected(tmp_path: Path) -> None:
    path = tmp_path / "noise.h264"
    path.write_bytes(b"\x00\x01\x02\x03" * 100)
    with pytest.raises((CodecConflictError, StreamError)):
        sniff_codec(path)


def test_audio_track_limits(h264_bundle) -> None:
    audio = load_audio_track(h264_bundle.audio_path, limits=Limits())
    assert audio.sample_rate == 48_000
    assert audio.frames > 0
    with pytest.raises(InputError):
        load_audio_track(
            h264_bundle.audio_path, limits=Limits(max_audio_channels=0)
        )


def test_audio_rejects_negative_start(h264_bundle) -> None:
    with pytest.raises(InputError):
        load_audio_track(h264_bundle.audio_path, start_pts_ticks=-1)
