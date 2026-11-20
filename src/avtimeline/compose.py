"""Compose the timeline into one program for the encoder.

Video frames are rendered at the timeline rate from the normalized lossless
intermediates; adjacent clips are cross-faded inside their transition span.
Audio stems from every audio-bearing track are laid out at 48 kHz, cross-faded
across transitions and mixed.  Rendering is bounded by an explicit frame guard
so a runaway timeline cannot exhaust the memory budget.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any

import numpy as np

from .errors import MediaError
from .model import Rate, Span
from .normalize import NormalizedAudio, NormalizedVideo
from .timeline import Clip, Timeline, Track

MAX_PROGRAM_FRAMES = 900
PROGRAM_WIDTH = 640
PROGRAM_HEIGHT = 360
PROGRAM_AUDIO_RATE = 48_000
VIDEO_TRACK_KINDS = {"video", "audiovisual"}
AUDIO_TRACK_KINDS = {"audio", "audiovisual"}
BLACK_FRAME = np.zeros((PROGRAM_HEIGHT, PROGRAM_WIDTH, 3), dtype=np.uint8)


@dataclass(frozen=True)
class Program:
    """Composed program fed to the encoder."""

    video_frames: list[np.ndarray]
    audio: np.ndarray
    rate: Rate
    audio_rate: int = PROGRAM_AUDIO_RATE
    width: int = PROGRAM_WIDTH
    height: int = PROGRAM_HEIGHT
    window: Span = Span(Fraction(0), Fraction(0))
    video_end: Fraction = Fraction(0)
    audio_end: Fraction = Fraction(0)

    @property
    def frame_count(self) -> int:
        return len(self.video_frames)

    @property
    def audio_samples(self) -> int:
        return int(self.audio.shape[1])

    def frame(self, index: int) -> np.ndarray | None:
        if 0 <= index < len(self.video_frames):
            return self.video_frames[index]
        return None


@dataclass
class ComposeOutcome:
    program: Program | None = None
    errors: list[MediaError] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    video_transitions: list[dict[str, Any]] = field(default_factory=list)


def primary_video_track(timeline: Timeline) -> Track | None:
    """The track whose pictures become the program video."""
    for track in timeline.tracks.values():
        if track.kind in VIDEO_TRACK_KINDS and track.clips:
            return track
    return None


def clip_at(track: Track, moment: Fraction) -> Clip | None:
    for clip in track.clips:
        if clip.start <= moment < clip.end:
            return clip
    return None


def transition_at(track: Track, moment: Fraction):
    for transition in track.transitions:
        if transition.span.start <= moment < transition.span.end:
            return transition
    return None


def sample_at(time: Fraction, rate: int) -> int:
    """Round to the nearest sample, ties to even (deterministic)."""
    units = time * rate
    lower = units.numerator // units.denominator
    remainder = units - lower
    if remainder > Fraction(1, 2):
        return lower + 1
    if remainder == Fraction(1, 2):
        return lower if lower % 2 == 0 else lower + 1
    return lower


def frame_at(time: Fraction, rate: Rate) -> int:
    units = time * rate.value
    return max(0, units.numerator // units.denominator)


def blend_images(outgoing: np.ndarray, incoming: np.ndarray, numerator: int, denominator: int) -> np.ndarray:
    """Integer-weighted cross-fade, deterministic and rounding-stable."""
    if numerator <= 0:
        return outgoing
    if numerator >= denominator:
        return incoming
    weight = denominator - numerator
    mixed = (
        outgoing.astype(np.int32) * weight + incoming.astype(np.int32) * numerator
    ) // denominator
    return mixed.astype(np.uint8)


def compose_program(
    timeline: Timeline,
    sources_video: dict[str, NormalizedVideo],
    sources_audio: dict[str, NormalizedAudio],
    window: Span,
    audio_gains: dict[str, float] | None = None,
) -> ComposeOutcome:
    """Render the program for ``window`` from normalized sources."""
    from .compose_impl import render

    return render(timeline, sources_video, sources_audio, window, audio_gains)
