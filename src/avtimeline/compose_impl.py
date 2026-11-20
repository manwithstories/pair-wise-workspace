"""Rendering implementation for :mod:`avtimeline.compose`.

Kept separate so the public data types stay small.  The renderer loads only the
frames it needs, cross-fades both video and audio inside transition spans, and
never exceeds the frame guard.
"""

from __future__ import annotations

from fractions import Fraction
from typing import Any

import av
import numpy as np
import soundfile as sf

from .errors import MediaError, MediaLocation, Severity, Stage
from .model import Rate, Span
from .timeline import Clip, Timeline, Track

from .compose import (
    AUDIO_TRACK_KINDS,
    BLACK_FRAME,
    MAX_PROGRAM_FRAMES,
    PROGRAM_AUDIO_RATE,
    PROGRAM_HEIGHT,
    PROGRAM_WIDTH,
    ComposeOutcome,
    Program,
    blend_images,
    clip_at,
    frame_at,
    primary_video_track,
    sample_at,
    transition_at,
)


def _load_video_frames(path: str, limit: int) -> list[np.ndarray]:
    container = av.open(path)
    try:
        frames: list[np.ndarray] = []
        for frame in container.decode(video=0):
            if len(frames) >= limit:
                break
            rgb = frame.reformat(
                width=PROGRAM_WIDTH, height=PROGRAM_HEIGHT, format="rgb24"
            ).to_ndarray()
            frames.append(np.ascontiguousarray(rgb))
        return frames
    finally:
        container.close()


def _load_audio(path: str) -> tuple[np.ndarray, int]:
    with sf.SoundFile(path) as handle:
        data = handle.read(dtype="float32", always_2d=True)
        rate = handle.samplerate
    return np.ascontiguousarray(data.T), rate


def render(
    timeline: Timeline,
    sources_video: dict[str, Any],
    sources_audio: dict[str, Any],
    window: Span,
    audio_gains: dict[str, float] | None = None,
) -> ComposeOutcome:
    outcome = ComposeOutcome()
    track = primary_video_track(timeline)
    if track is None:
        outcome.errors.append(
            MediaError(
                code="no_video_track",
                stage=Stage.TRANSITION,
                message="timeline has no video-bearing track",
                severity=Severity.FATAL,
            )
        )
        return outcome
    first_source = sources_video.get(track.clips[0].source)
    if first_source is None:
        outcome.errors.append(
            MediaError(
                code="missing_normalized_video",
                stage=Stage.TRANSITION,
                message=f"no normalized video for source {track.clips[0].source!r}",
                location=MediaLocation(track=track.id, pts=track.clips[0].start),
                severity=Severity.FATAL,
            )
        )
        return outcome
    rate = first_source.spec.rate
    step = rate.frame_duration
    video_end = min(window.end, track.span.end if track.span else window.end)
    span = video_end - window.start
    frame_count = int(span / step) if span > 0 else 0
    if frame_count > MAX_PROGRAM_FRAMES:
        outcome.errors.append(
            MediaError(
                code="program_too_long",
                stage=Stage.TRANSITION,
                message=(
                    f"window needs {frame_count} frames, above the {MAX_PROGRAM_FRAMES} "
                    "frame memory guard"
                ),
                location=MediaLocation(track=track.id, pts=window.start),
                severity=Severity.FATAL,
            )
        )
        return outcome
    decoded: dict[str, list[np.ndarray]] = {}
    for clip in track.clips:
        if clip.source in decoded:
            continue
        normalized = sources_video.get(clip.source)
        if normalized is None:
            outcome.errors.append(
                MediaError(
                    code="missing_normalized_video",
                    stage=Stage.TRANSITION,
                    message=f"no normalized video for source {clip.source!r}",
                    location=MediaLocation(track=track.id, pts=clip.start),
                    severity=Severity.FATAL,
                )
            )
            continue
        decoded[clip.source] = _load_video_frames(normalized.path, MAX_PROGRAM_FRAMES)
    frames: list[np.ndarray] = []
    for index in range(frame_count):
        moment = window.start + index * step
        frames.append(_render_frame(track, decoded, moment, rate, step, outcome))
    audio = _mix_audio(timeline, sources_audio, window, audio_gains, outcome)
    if audio is None:
        return outcome
    video_span = Span(window.start, window.start + Fraction(frame_count) * step)
    audio_span_end = window.start + Fraction(audio.shape[1], PROGRAM_AUDIO_RATE)
    outcome.program = Program(
        video_frames=frames,
        audio=audio,
        rate=rate,
        window=window,
        video_end=video_span.end,
        audio_end=audio_span_end,
    )
    outcome.notes.append(f"program: {len(frames)} frames, {audio.shape[1]} samples")
    return outcome


def _render_frame(
    track: Track,
    decoded: dict[str, list[np.ndarray]],
    moment: Fraction,
    rate: Rate,
    step: Fraction,
    outcome: ComposeOutcome,
) -> np.ndarray:
    clip = clip_at(track, moment)
    if clip is None:
        return BLACK_FRAME
    transition = transition_at(track, moment)
    if transition is not None:
        after = next((c for c in track.clips if c.id == transition.after), None)
        before = next((c for c in track.clips if c.id == transition.before), None)
        if after is not None and before is not None:
            denominator = max(1, int(round(transition.span.duration / step)))
            prefix = int(round((moment - transition.span.start) / step))
            numerator = max(0, min(prefix, denominator))
            out_img = _frame_for(after, decoded, moment, rate)
            in_img = _frame_for(before, decoded, moment, rate)
            return blend_images(out_img, in_img, numerator, denominator)
    return _frame_for(clip, decoded, moment, rate)


def _frame_for(
    clip: Clip, decoded: dict[str, list[np.ndarray]], moment: Fraction, rate: Rate
) -> np.ndarray:
    frames = decoded.get(clip.source, [])
    local = clip.source_in + (moment - clip.start)
    index = frame_at(local, rate)
    if not frames:
        return BLACK_FRAME
    if index >= len(frames):
        return frames[-1]
    return frames[index]


def _mix_audio(
    timeline: Timeline,
    sources_audio: dict[str, Any],
    window: Span,
    gains: dict[str, float] | None,
    outcome: ComposeOutcome,
) -> np.ndarray | None:
    total = sample_at(window.end, PROGRAM_AUDIO_RATE) - sample_at(window.start, PROGRAM_AUDIO_RATE)
    if total <= 0:
        return np.zeros((2, 0), dtype=np.float32)
    gains = gains or {}
    mix = np.zeros((2, total), dtype=np.float32)
    used = 0
    origin = sample_at(window.start, PROGRAM_AUDIO_RATE)
    for track in timeline.tracks.values():
        if track.kind not in AUDIO_TRACK_KINDS or not track.clips:
            continue
        if all(clip.source not in sources_audio for clip in track.clips):
            continue
        loaded: dict[str, np.ndarray] = {}
        for clip in track.clips:
            if clip.source not in sources_audio:
                continue
            if clip.source not in loaded:
                data, rate = _load_audio(sources_audio[clip.source].path)
                if rate != PROGRAM_AUDIO_RATE:
                    outcome.errors.append(
                        MediaError(
                            code="audio_rate_mismatch",
                            stage=Stage.TRANSITION,
                            message=f"normalized audio {clip.source!r} is {rate} Hz",
                            location=MediaLocation(track=track.id, pts=clip.start),
                        )
                    )
                    continue
                loaded[clip.source] = data
        if not loaded:
            continue
        stem = np.zeros((2, total), dtype=np.float32)
        for clip in track.clips:
            if clip.source in loaded:
                _lay_clip(loaded[clip.source], clip, origin, total, stem)
        for transition in track.transitions:
            _crossfade(loaded, track, transition, origin, total, stem)
        gain = float(gains.get(track.id, 1.0))
        if track.kind == "audiovisual":
            gain *= float(gains.get("_camera_stem", 0.9))
        mix += stem * np.float32(max(0.0, gain))
        used += 1
    if used == 0:
        return np.zeros((2, total), dtype=np.float32)
    np.clip(mix, -1.0, 1.0, out=mix)
    return mix


def _lay_clip(
    data: np.ndarray,
    clip: Clip,
    origin: int,
    total: int,
    mix: np.ndarray,
) -> None:
    start = max(0, sample_at(clip.start, PROGRAM_AUDIO_RATE) - origin)
    end = min(total, sample_at(clip.end, PROGRAM_AUDIO_RATE) - origin)
    if end <= start:
        return
    offset = sample_at(clip.source_in, PROGRAM_AUDIO_RATE)
    count = end - start
    indices = offset + np.arange(count, dtype=np.int64)
    valid = indices < data.shape[1]
    if not np.all(valid):
        indices = indices[valid]
        count = indices.size
    if count <= 0:
        return
    mix[:, start : start + count] += data[:, indices]


def _crossfade(
    loaded: dict[str, np.ndarray],
    track: Track,
    transition: Any,
    origin: int,
    total: int,
    mix: np.ndarray,
) -> None:
    after = next((c for c in track.clips if c.id == transition.after), None)
    before = next((c for c in track.clips if c.id == transition.before), None)
    if after is None or before is None:
        return
    if after.source not in loaded or before.source not in loaded:
        return
    start = max(0, sample_at(transition.span.start, PROGRAM_AUDIO_RATE) - origin)
    end = min(total, sample_at(transition.span.end, PROGRAM_AUDIO_RATE) - origin)
    length = end - start
    if length <= 0:
        return
    positions = np.arange(start, end, dtype=np.int64)
    weights = (np.arange(length, dtype=np.float32) + 0.5) / float(length)
    out_data = loaded[after.source]
    in_data = loaded[before.source]
    out_idx = (
        sample_at(after.source_in, PROGRAM_AUDIO_RATE)
        + positions
        + origin
        - sample_at(after.start, PROGRAM_AUDIO_RATE)
    )
    in_idx = (
        sample_at(before.source_in, PROGRAM_AUDIO_RATE)
        + positions
        + origin
        - sample_at(before.start, PROGRAM_AUDIO_RATE)
    )
    out_idx = np.clip(out_idx, 0, out_data.shape[1] - 1)
    in_idx = np.clip(in_idx, 0, in_data.shape[1] - 1)
    mix[:, start:end] = out_data[:, out_idx] * (1.0 - weights) + in_data[:, in_idx] * weights
