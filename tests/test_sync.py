"""Synchronization tests: anchor alignment, drift limits, VFR and error intervals."""

from __future__ import annotations

from dataclasses import replace
from fractions import Fraction

import pytest

from mediasync.clock import TICKS_PER_SECOND
from mediasync.errors import InputError
from mediasync.ingest import Anchor, AudioTrack, PacketRecord, VideoTrack
from mediasync.recover import Segment
from mediasync.sync import (
    SyncConfig,
    align_audio_only,
    align_track,
    detect_fps_changes,
    implied_frame_rate,
    sample_position,
)


def make_video(*, frame_count: int = 60, nominal: Fraction = Fraction(30), offset_ticks: int = 0,
               drift_ppm: float = 0.0, duration_override: int | None = None) -> VideoTrack:
    step = int(Fraction(TICKS_PER_SECOND) / nominal)
    packets = []
    for index in range(frame_count):
        duration = step
        if duration_override is not None and index == frame_count // 2:
            duration = duration_override
        pts = offset_ticks + int(round(index * step * (1.0 + drift_ppm / 1_000_000.0)))
        packets.append(
            PacketRecord(
                index=index,
                offset=index * 100,
                size=100,
                keyframe=index % 10 == 0,
                pts_ticks=pts,
                duration_ticks=duration,
                seq=index,
            )
        )
    return VideoTrack(
        track_id="cam",
        role="camera",
        codec="h264",
        path=__file__.encode() and __import__("pathlib").Path("/nonexistent"),
        stream_size=frame_count * 100,
        nominal_rate=nominal,
        width=320,
        height=180,
        packets=tuple(packets),
    )


def make_audio(*, sample_rate: int = 48_000, seconds: float = 3.0) -> AudioTrack:
    return AudioTrack(
        track_id="program",
        role="program",
        path=__import__("pathlib").Path("/nonexistent"),
        sample_rate=sample_rate,
        channels=1,
        frames=int(seconds * sample_rate),
        start_pts_ticks=0,
    )


def make_anchors(
    seconds,
    *,
    fps: Fraction = Fraction(30),
    video_shift: float = 0.0,
    audio_shift: float = 0.0,
) -> tuple:
    video = tuple(
        Anchor(
            label=f"a{index}",
            video_index=int(round(value * float(fps))),
            audio_sample=0,
            video_seconds=value,
            audio_seconds=value + video_shift,
        )
        for index, value in enumerate(seconds)
    )
    audio = tuple(
        Anchor(
            label=f"a{index}",
            video_index=0,
            audio_sample=int(round((value + audio_shift) * 48_000)),
            video_seconds=value,
            audio_seconds=value + audio_shift,
        )
        for index, value in enumerate(seconds)
    )
    return video, audio


def test_alignment_with_matching_anchors() -> None:
    video = make_video()
    audio = make_audio()
    video.anchors, audio.anchors = make_anchors([0.5, 1.0, 1.5])
    sync = align_track(video, audio)
    assert sync.aligned
    assert sync.max_error_ticks <= SyncConfig().max_error_ticks
    assert sync.anchors == 3


def test_large_offset_is_reported_not_clamped() -> None:
    video = make_video(offset_ticks=int(0.030 * TICKS_PER_SECOND))
    audio = make_audio()
    video.anchors, audio.anchors = make_anchors([0.5, 1.0, 1.5])
    sync = align_track(video, audio)
    assert not sync.aligned
    assert sync.max_error_ms == pytest.approx(30.0, abs=1.0)
    assert "error-over-tolerance" in sync.reason
    assert sync.error_intervals
    assert all(item.reason == "anchor-error" for item in sync.error_intervals)


def test_drift_beyond_limit_is_rejected() -> None:
    video = make_video(drift_ppm=4000.0)
    audio = make_audio()
    video.anchors, audio.anchors = make_anchors([0.5, 1.0, 1.5])
    sync = align_track(video, audio)
    assert not sync.aligned
    assert abs(sync.drift_ppm) > SyncConfig().drift_limit_ppm
    assert any("drift" in item.reason for item in sync.error_intervals)


def test_frame_rate_change_is_reported() -> None:
    video = make_video(duration_override=9000)
    audio = make_audio()
    video.anchors, audio.anchors = make_anchors([0.5, 1.0, 1.5])
    sync = align_track(video, audio)
    assert not sync.aligned
    assert sync.fps_changes >= 1
    assert any(item.reason == "fps-change" for item in sync.error_intervals)


def test_insufficient_anchors_are_rejected() -> None:
    video = make_video()
    audio = make_audio()
    video.anchors, audio.anchors = make_anchors([0.5])
    sync = align_track(video, audio)
    assert not sync.aligned
    assert sync.reason == "insufficient-anchors"


def test_incomplete_segment_short_circuits_alignment() -> None:
    video = make_video()
    audio = make_audio()
    video.anchors, audio.anchors = make_anchors([0.5, 1.0])
    segment = Segment(
        interval_id="v@seg0",
        track_id="cam",
        start_ticks=0,
        end_ticks=5000,
        complete=False,
        reason="packet-loss",
    )
    sync = align_track(video, audio, segment=segment)
    assert not sync.aligned
    assert sync.reason.startswith("unrecoverable-segment")


def test_out_of_range_anchor_is_rejected() -> None:
    video = make_video()
    audio = make_audio()
    video.anchors = (
        Anchor("a0", video_index=0, video_seconds=0.5),
        Anchor("a1", video_index=9999, video_seconds=0.5),
    )
    audio.anchors = (Anchor("a0", audio_sample=0), Anchor("a1", audio_sample=0))
    with pytest.raises(InputError):
        align_track(video, audio)


def test_detect_fps_changes_matches_nominal() -> None:
    assert detect_fps_changes([3000, 3000, 3000], 3000, 0.02) == []
    changes = detect_fps_changes([3000, 4500], 3000, 0.02)
    assert changes == [(1, 4500)]


def test_audio_only_alignment() -> None:
    audio = make_audio()
    sync = align_audio_only(audio, 0)
    assert sync.aligned
    assert sync.kind == "audio"
    shifted = make_audio()
    shifted.start_pts_ticks = int(0.05 * TICKS_PER_SECOND)
    sync = align_audio_only(shifted, 0)
    assert not sync.aligned
    assert sync.reason == "audio-offset-over-tolerance"


def test_sample_position_conversion() -> None:
    sync = align_audio_only(make_audio(), 0)
    assert sample_position(sync, 0, 48_000) == 0
    assert sample_position(sync, TICKS_PER_SECOND, 48_000) == 48_000


def test_implied_frame_rate() -> None:
    assert implied_frame_rate(30, TICKS_PER_SECOND) == 30
    with pytest.raises(InputError):
        implied_frame_rate(0, 100)


def test_sync_config_validation() -> None:
    with pytest.raises(InputError):
        SyncConfig(max_error_ms=0)
    with pytest.raises(InputError):
        SyncConfig(drift_limit_ppm=0)
    with pytest.raises(InputError):
        SyncConfig(fps_change_tolerance=1.0)
