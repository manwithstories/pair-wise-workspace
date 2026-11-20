"""Offscreen composition with exact source-span tracking.

Composition model
-----------------
For every output frame ``n`` the renderer asks each *active* track (a track
referenced by at least one clip - unreferenced tracks are excluded upstream):

1. which clip of that track covers the output frame's timestamp, and
2. which source frame of that track corresponds to that timestamp.

Video clips map timeline ticks to *source frame numbers* through exact
rational arithmetic in the track's frame clock.  Audio clips map timeline
ticks to *source sample numbers* in the track's sample clock.  The
single-source sample-rate case therefore stays on exact integers; the
multi-source case uses one resampling pass per clip.

The renderer keeps the exact ``(track, clip, source_frame, source_byte_range)``
provenance of every output frame, plus a ``recovered`` flag that is true only
when the contributing video frame was genuinely recovered (retried), never for
concealed frames.

Sync measurement
----------------
:class:`SyncTracker` correlates the decoded audio against the decoded video
luma signal in the overlap window and reports the lag with sub-frame
resolution.  The reference signal is the mean luma of each video frame; the
audio signal is the block mean of the PCM.  Because both derive from the same
patterned fixtures, the correlation peak is the offset actually present in the
output, which is what the QC report must state.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Iterator

import numpy as np

from .bitstream import Codec
from .clock import Rounding, TimeBase, round_rational
from .decode import (
    SOURCE_CONCEALED,
    AudioReader,
    DecodedFrame,
    DecodeStats,
    FrameBlock,
    LossPolicy,
    decode_video_blocks,
    resample_window,
)
from .errors import ErrorCategory, make_error
from .timeline import Clip, Timeline, Track, TrackKind

__all__ = [
    "SourceSpan",
    "RenderedFrame",
    "VideoSource",
    "AudioSource",
    "pixel_hash",
]

# Frames are converted to this pixel hash domain so exports are byte-exact.
PIXEL_HASH_DOMAIN = b"bcastqc/rgb24/v1\x00"


def pixel_hash(array: np.ndarray) -> str:
    digest = hashlib.sha256(PIXEL_HASH_DOMAIN)
    digest.update(np.ascontiguousarray(array, dtype=np.uint8).tobytes())
    return digest.hexdigest()


@dataclass(frozen=True)
class SourceSpan:
    """Where one output frame (or audio block) came from."""

    track: str
    clip: str
    source_index: int          # source frame or sample index
    source_offset: int         # byte offset in the source file
    source_size: int
    recovered: bool = False
    concealed: bool = False
    gap_ms: Fraction = Fraction(0)
    # provenance of a genuine recovery ('retry'), never set for concealment
    recovery_source: str | None = None
    # what the span represents when no source frame existed: 'truncated',
    # 'missing_keyframe', or 'concealed'
    cause: str | None = None

    def to_json(self) -> dict:
        payload = {
            "track": self.track,
            "clip": self.clip,
            "source_index": self.source_index,
            "byte_range": [self.source_offset, self.source_offset + self.source_size],
            "recovered": self.recovered,
        }
        if self.recovery_source:
            payload["recovery_source"] = self.recovery_source
        if self.cause:
            payload["cause"] = self.cause
        if self.concealed:
            payload["concealed"] = True
            payload["gap_ms"] = float(self.gap_ms)
        return payload


@dataclass
class RenderedFrame:
    number: int
    pts_ticks: int              # timeline time base
    time_base: TimeBase
    array: np.ndarray
    spans: tuple[SourceSpan, ...]
    audio: np.ndarray | None = None   # samples for this frame's interval, shape (n, ch)

    @property
    def time_seconds(self) -> Fraction:
        return Fraction(self.pts_ticks, 1) * self.time_base.seconds_per_tick

    @property
    def recovered(self) -> bool:
        return any(span.recovered for span in self.spans)

    @property
    def concealed(self) -> bool:
        return any(span.concealed for span in self.spans)

    @property
    def hash(self) -> str:
        return pixel_hash(self.array)

    def to_json(self) -> dict:
        return {
            "frame": self.number,
            "time_ticks": self.pts_ticks,
            "time_base": str(self.time_base),
            "time_seconds": round(float(self.time_seconds), 9),
            "pixel_hash": self.hash,
            "sources": [span.to_json() for span in self.spans],
            "recovered": self.recovered,
            "concealed": self.concealed,
        }


class VideoSource:
    """One decoded video track, indexed by source frame number."""

    def __init__(self, track: Track, timeline: Timeline, *, block_frames: int = 8,
                 policy: LossPolicy | None = None) -> None:
        self.track = track
        self.timeline = timeline
        self.frame_rate = timeline.track_frame_rate(track.id)
        self.block_frames = block_frames
        self.policy = policy
        codec = track.codec
        if codec is None:
            from .decode import probe_stream

            codec, _ = probe_stream(track.path)
        self.codec: Codec = codec
        self.stats = DecodeStats()
        self.unit_count: int | None = None

    def blocks(self) -> Iterator[FrameBlock]:
        return decode_video_blocks(
            self.track.path,
            self.codec,
            self.frame_rate,
            block_frames=self.block_frames,
            policy=self.policy,
            stats=self.stats,
            total_units=self.unit_count,
        )


class AudioSource:
    """One PCM/WAV track addressed in source samples."""

    def __init__(self, track: Track, timeline: Timeline) -> None:
        if track.audio is None:
            raise make_error(ErrorCategory.AUDIO_FORMAT, "audio track lacks a layout", track=track.id)
        self.track = track
        self.timeline = timeline
        self.layout = track.audio

    def reader(self, channels: int) -> AudioReader:
        return AudioReader(self.track.path, channels=channels)
