"""Canned fixture profiles used by tests and the CLI demo."""

from __future__ import annotations

from .fixture_builder import FixtureSpec, FixtureTrack


# ── canned profiles ───────────────────────────────────────────────────────


def demo_spec() -> FixtureSpec:
    """The acceptance-demo profile: 2 tracks, hostile timestamps, real Cues.

    Deliberately includes what a bad muxer emits: negative first frames, a
    duplicated presentation time, a B-frame style presentation reorder,
    unknown-size Segment and Clusters, and cue positions that must be
    recomputed on rewrite.
    """
    video_ticks = [-80, -40, 0, 80, 40, 40]
    audio_ticks = [-80, -59, -38, -17, 4, 25, 46, 67]
    return FixtureSpec(
        scale_ns=1_000_000,
        duration_ticks=240.0,
        unknown_segment=True,
        unknown_clusters=True,
        include_cues=True,
        include_attachments=True,
        tracks=[
            FixtureTrack(
                number=1,
                uid=0xDEADBEEF001,
                kind="video",
                codec_id="V_MPEG4/ISO/AVC",
                default_duration_ns=40_000_000,
                ticks=video_ticks,
                width=640,
                height=360,
                codec_private=bytes.fromhex("0164001fffe1001a6764001facd9405005bb011000000300100000030320f183196001000468eb8f2c"),
            ),
            FixtureTrack(
                number=2,
                uid=0xDEADBEEF002,
                kind="audio",
                codec_id="A_AAC",
                language="eng",
                default=True,
                ticks=audio_ticks,
                sample_rate=48_000.0,
                channels=2,
                codec_private=bytes.fromhex("121056e500"),
            ),
        ],
        samples_per_cluster=8,
        empty_cluster_at_start=False,
    )


def subtitle_spec() -> FixtureSpec:
    """Three tracks including subtitles and a file without Cues or durations."""
    return FixtureSpec(
        scale_ns=1_000_000,
        unknown_segment=True,
        unknown_clusters=True,
        tracks=[
            FixtureTrack(
                number=1,
                uid=11,
                kind="video",
                codec_id="V_MPEGH/ISO/HEVC",
                ticks=[0, 40, 80],
                default_duration_ns=40_000_000,
            ),
            FixtureTrack(number=2, uid=22, kind="audio", codec_id="A_OPUS", ticks=[0, 20, 40, 60]),
            FixtureTrack(
                number=3,
                uid=33,
                kind="subtitle",
                codec_id="S_TEXT/UTF8",
                language="jpn",
                default=False,
                ticks=[0, 500],
                payload_size=12,
            ),
        ],
        samples_per_cluster=3,
    )


def minimal_spec() -> FixtureSpec:
    """A tiny known-size fixture for fast structural tests."""
    return FixtureSpec(
        scale_ns=1_000_000,
        unknown_segment=False,
        unknown_clusters=False,
        tracks=[
            FixtureTrack(number=1, uid=1, kind="video", codec_id="V_MPEG4/ISO/AVC", ticks=[0, 40]),
        ],
        samples_per_cluster=2,
    )


__all__ = ["demo_spec", "minimal_spec", "subtitle_spec"]
