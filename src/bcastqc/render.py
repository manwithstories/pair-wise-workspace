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
    "SyncTracker",
    "Renderer",
    "VideoSource",
    "AudioSource",
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


# ==========================================================================
# Sync tracking
# ==========================================================================


@dataclass
class SyncTracker:
    """Records what the QC report needs to measure audio/video alignment.

    Per output frame it keeps:

    * ``frame_hashes`` - the exact pixel hash, so a clean reference pass can
      establish picture identity (and therefore duplication / displacement)
      without relying on any similarity heuristic;
    * ``luma`` and ``audio_centre`` - the marker signals used to measure
      video and audio displacement independently.
    """

    frame_times: list[Fraction] = field(default_factory=list)
    luma: list[float] = field(default_factory=list)
    audio_level: list[float] = field(default_factory=list)
    audio_centre: list[float] = field(default_factory=list)
    frame_hashes: list[str] = field(default_factory=list)

    def add_frame(self, time: Fraction, frame: np.ndarray) -> None:
        self.frame_times.append(time)
        array = np.asarray(frame)
        self.luma.append(float(array.astype(np.float32).mean()))
        self.frame_hashes.append(pixel_hash(array))

    def add_audio(self, block: np.ndarray | None) -> None:
        if block is None or block.size == 0:
            self.audio_level.append(0.0)
            self.audio_centre.append(0.0)
            return
        values = np.asarray(block, dtype=np.float64)
        self.audio_level.append(float(np.sqrt(np.mean(np.square(values)))))
        n = values.shape[0]
        third = max(1, n // 3)
        centre = values[n // 2 - third // 2: n // 2 + third - third // 2]
        self.audio_centre.append(float(np.sqrt(np.mean(np.square(centre)))))

    def sync_error_ms(self, max_lag_frames: int = 12, *,
                     around: int | None = None, radius: int = 2) -> tuple[Fraction | None, int]:
        """Estimate the audio-video lag by normalised cross-correlation.

        Returns ``(signed_error_ms, lag_frames)``; positive means audio lags
        video.  The estimate is deterministic (no random resampling).
        ``None`` is returned for the error when the window has no signal
        variance, which callers must treat as "no new evidence".

        ``around`` restricts the search to ``around +- radius``.  Short windows
        have too few samples for a wide search: unrelated lags can score
        highly by chance.  The caller therefore estimates a coarse global lag
        once and refines each window locally, which also tracks genuine drift.
        """
        n = min(len(self.luma), len(self.audio_level))
        if n < 4:
            return None, 0
        video = np.asarray(self.luma[:n], dtype=np.float64)
        audio = np.asarray(self.audio_centre[:n], dtype=np.float64)
        video = video - video.mean()
        audio = audio - audio.mean()
        v_norm = np.linalg.norm(video)
        a_norm = np.linalg.norm(audio)
        if v_norm == 0 or a_norm == 0:
            return None, 0
        video /= v_norm
        audio /= a_norm
        if around is None:
            lags = range(-max_lag_frames, max_lag_frames + 1)
        else:
            lags = range(around - radius, around + radius + 1)
        best_lag = around if around is not None else 0
        best_score = -np.inf
        # the zero-lag score is the reference for accepting a refinement
        zero_score = float(np.dot(video, audio))
        for lag in lags:
            if lag >= 0:
                left = video[: n - lag]
                right = audio[lag:]
            else:
                left = video[-lag:]
                right = audio[: n + lag]
            if left.size < max(4, n // 2):
                continue  # too little overlap: not enough evidence
            overlap = left.size
            score = float(np.dot(left, right)) * (n / overlap)  # normalise overlap
            if score > best_score + 1e-12:
                best_score = score
                best_lag = lag
        if around is not None and best_lag != around:
            # accept a local shift only when it beats the previous lag by a
            # clear margin; otherwise keep the established alignment
            reference = zero_score
            if best_score < reference + 0.05:
                best_lag = around
        if len(self.frame_times) >= 2:
            step = self.frame_times[1] - self.frame_times[0]
        else:
            step = Fraction(1, 25)
        return step * best_lag * 1000, best_lag

    def _slice(self, start: int, end: int) -> "SyncTracker":
        """A copy restricted to frames ``[start, end)`` for windowed estimates."""
        end = min(end, len(self.luma), len(self.audio_level))
        start = max(0, min(start, end))
        window = SyncTracker()
        window.frame_times = list(self.frame_times[start:end])
        window.luma = list(self.luma[start:end])
        window.audio_level = list(self.audio_level[start:end])
        window.audio_centre = list(self.audio_centre[start:end])
        window.frame_hashes = list(self.frame_hashes[start:end])
        return window

    def to_json(self) -> dict:
        error_ms, lag = self.sync_error_ms()
        return {
            "samples": len(self.luma),
            "sync_error_ms": float(error_ms) if error_ms is not None else 0.0,
            "lag_frames": lag,
        }


# ==========================================================================
# Streaming cursors
# ==========================================================================


class _VideoCursor:
    """Forward-only access to source frames of one video track.

    Holds at most ``window`` frames.  A backwards request (a clip whose
    source range precedes the previous one) restarts the track's decoder;
    this is bounded in memory but costs re-decoding, which is the documented
    trade-off for arbitrary clip order.
    """

    def __init__(self, source: VideoSource, *, window: int = 32) -> None:
        self.source = source
        self.window = max(window, source.block_frames * 2)
        self._blocks: Iterator[FrameBlock] | None = None
        self._buffer: dict[int, DecodedFrame] = {}
        self._oldest = 0
        self._exhausted = False
        self.restarts = 0
        self.last_good: DecodedFrame | None = None
        self.frames_seen = 0
        self.exhausted_cause = "truncated"

    def _start(self) -> None:
        self._blocks = self.source.blocks()
        self._buffer.clear()
        self._oldest = 0
        self._exhausted = False

    def _pull(self) -> bool:
        if self._blocks is None:
            self._start()
        while not self._exhausted:
            try:
                block = next(self._blocks)  # type: ignore[arg-type]
            except StopIteration:
                self._exhausted = True
                return False
            for frame in block.frames:
                self._buffer[frame.pts] = frame
                self.frames_seen += 1
            if self._buffer:
                return True
        return False

    def get(self, frame_index: int) -> DecodedFrame | None:
        if frame_index < 0:
            return None
        if frame_index < self._oldest:
            # backwards seek: restart the track decoder
            self._start()
            self.restarts += 1
        while True:
            if frame_index in self._buffer:
                frame = self._buffer.pop(frame_index)
                self.last_good = frame
                self._trim(frame_index + 1)
                return frame
            if self._exhausted:
                self._trim(frame_index + 1)
                return None
            if self._buffer and max(self._buffer) < frame_index:
                # requested frame was concealed or does not exist; drop older
                self._trim(frame_index)
            if not self._pull():
                return None

    def _trim(self, keep_from: int) -> None:
        self._oldest = max(self._oldest, keep_from)
        for pts in [p for p in self._buffer if p < keep_from]:
            self._buffer.pop(pts, None)


class _AudioCursor:
    """Random-access reader for one audio track (seeks per output interval)."""

    def __init__(self, source: AudioSource, channels: int) -> None:
        self.source = source
        self.reader = source.reader(channels)
        self.sample_rate = self.reader.sample_rate
        self.frames_total = self.reader.frames_total

    def read(self, start_seconds: Fraction, count: int, target_rate: int) -> tuple[np.ndarray, int]:
        start_sample = round_rational(start_seconds * self.sample_rate, Rounding.FLOOR)
        if target_rate == self.sample_rate:
            return self.reader.read_window(start_sample, count)
        span = int(np.ceil(count * self.sample_rate / target_rate)) + 2
        raw, padded = self.reader.read_window(start_sample, span)
        resampled = resample_window(raw, self.sample_rate, target_rate)
        out = np.zeros((count, raw.shape[1]), dtype=np.float32)
        take = min(count, resampled.shape[0])
        out[:take] = resampled[:take]
        return out, max(0, padded - max(0, span - take))

    def close(self) -> None:
        self.reader.close()


# ==========================================================================
# Renderer
# ==========================================================================


@dataclass
class RenderStats:
    frames: int = 0
    recovered_frames: int = 0
    concealed_frames: int = 0
    video_restarts: int = 0
    audio_blocks: int = 0
    tail_padded_samples: int = 0
    negative_offset_clips: int = 0
    channel_conversions: int = 0

    def to_json(self) -> dict:
        return {
            "frames": self.frames,
            "recovered_frames": self.recovered_frames,
            "concealed_frames": self.concealed_frames,
            "video_restarts": self.video_restarts,
            "audio_blocks": self.audio_blocks,
            "tail_padded_samples": self.tail_padded_samples,
            "negative_offset_clips": self.negative_offset_clips,
            "channel_conversions": self.channel_conversions,
        }


class Renderer:
    """Offscreen compositor: timeline + decoded sources -> RenderedFrame stream.

    Only tracks referenced by clips participate; ``timeline.excluded_tracks``
    names any others.  Each output frame records every contributing source
    span, including byte ranges and recovery/concealment flags.
    """

    def __init__(
        self,
        timeline: Timeline,
        *,
        policies: dict[str, LossPolicy] | None = None,
        block_frames: int = 8,
    ) -> None:
        self.timeline = timeline
        self.block_frames = block_frames
        self.policies = policies or {}
        self.stats = RenderStats()
        self.resume_from_frame = 0
        self.skip_render = False
        active = timeline.active_tracks()
        self.video: dict[str, VideoSource] = {
            tid: VideoSource(track, timeline, block_frames=block_frames, policy=self.policies.get(tid))
            for tid, track in active.items()
            if track.kind is TrackKind.VIDEO
        }
        self.audio: dict[str, AudioSource] = {
            tid: AudioSource(track, timeline)
            for tid, track in active.items()
            if track.kind is TrackKind.AUDIO
        }
        self._cursors: dict[str, _VideoCursor] = {}
        self._audio_cursors: dict[str, _AudioCursor] = {}
        self._clip_by_track: dict[str, list[Clip]] = {
            tid: timeline.clips_for(tid) for tid in timeline.tracks
        }
        self.width = timeline.width
        self.height = timeline.height
        self._canvas = np.zeros((self.height, self.width, 3), dtype=np.uint8)

    # -- geometry --------------------------------------------------------
    def frame_count(self) -> int:
        """Frames needed to cover the whole timeline (final partial frame
        included).

        Using ceil keeps the audio sample total equal to the declared duration
        when the duration is not an exact multiple of the frame period (for
        example 2 s at 30000/1001 fps).  The final frame's audio block is
        clipped to the remaining samples, so no extra material is invented.
        """
        duration = self.timeline.duration().seconds()
        count = round_rational(duration * self.timeline.frame_rate, Rounding.CEIL)
        return max(0, int(count))

    def frame_time(self, number: int) -> Fraction:
        return Fraction(number, 1) / self.timeline.frame_rate

    def _clip_at(self, track_id: str, seconds: Fraction) -> Clip | None:
        for clip in self._clip_by_track.get(track_id, ()):
            start = self.timeline.ticks_to_seconds(clip.start_ticks)
            end = self.timeline.ticks_to_seconds(clip.end_ticks)
            if start <= seconds < end:
                return clip
        return None

    def _source_offset_seconds(self, track: Track, clip: Clip, seconds: Fraction) -> Fraction:
        """Seconds inside the *source* file for a timeline instant."""
        start = self.timeline.ticks_to_seconds(clip.start_ticks)
        source_in = self.timeline.ticks_to_seconds(clip.source_in_ticks)
        track_offset = self.timeline.ticks_to_seconds(track.offset_ticks)
        return source_in + (seconds - start) - track_offset

    # -- video -----------------------------------------------------------
    def _cursor(self, track_id: str) -> _VideoCursor:
        cursor = self._cursors.get(track_id)
        if cursor is None:
            cursor = _VideoCursor(self.video[track_id], window=self.block_frames * 4)
            self._cursors[track_id] = cursor
        return cursor

    def _video_for(self, seconds: Fraction) -> tuple[np.ndarray | None, list[SourceSpan]]:
        canvas = None
        spans: list[SourceSpan] = []
        for track_id in sorted(self.video):
            track = self.video[track_id].track
            clip = self._clip_at(track_id, seconds)
            if clip is None:
                continue
            offset = self._source_offset_seconds(track, clip, seconds)
            if offset < 0:
                self.stats.negative_offset_clips += 1
                continue
            frame_rate = self.video[track_id].frame_rate
            frame_index = round_rational(offset * frame_rate, Rounding.FLOOR)
            cursor = self._cursor(track_id)
            frame = cursor.get(frame_index)
            if frame is None:
                # The source ran out (truncation or a lost tail): record the
                # gap explicitly rather than a silent zero.
                last = cursor.last_good.pts if cursor.last_good is not None else -1
                gap_frames = max(1, frame_index - last)
                spans.append(
                    SourceSpan(
                        track=track_id,
                        clip=clip.id,
                        source_index=frame_index,
                        source_offset=0,
                        source_size=0,
                        concealed=True,
                        gap_ms=Fraction(gap_frames, 1) / self.video[track_id].frame_rate * 1000,
                        cause=cursor.exhausted_cause,
                    )
                )
                continue
            spans.append(
                SourceSpan(
                    track=track_id,
                    clip=clip.id,
                    source_index=frame_index,
                    source_offset=frame.source_offset,
                    source_size=frame.source_size,
                    recovered=frame.recovered,
                    concealed=frame.source == SOURCE_CONCEALED,
                    gap_ms=frame.gap_ms,
                    recovery_source=(frame.source if frame.recovered else None),
                    cause=("concealed" if frame.source == SOURCE_CONCEALED else None),
                )
            )
            canvas = self._composite(canvas, frame.array)
        return canvas, spans

    def _composite(self, base: np.ndarray | None, layer: np.ndarray) -> np.ndarray:
        if base is None:
            return layer
        h = min(base.shape[0], layer.shape[0])
        w = min(base.shape[1], layer.shape[1])
        out = base.copy()
        out[:h, :w] = layer[:h, :w]
        return out

    # -- audio -----------------------------------------------------------
    def _audio_for(self, seconds: Fraction, duration: Fraction) -> np.ndarray | None:
        # Exact per-frame sample count: difference of rounded absolute
        # positions, not a rounded duration.  At NTSC rates (30000/1001) a
        # rounded duration would accumulate ~0.4 samples per frame of drift.
        rate = self.timeline.output_sample_rate
        start = round_rational(seconds * rate, Rounding.FLOOR)
        end = round_rational((seconds + duration) * rate, Rounding.FLOOR)
        samples = max(0, end - start)
        if samples <= 0:
            return None
        mix = np.zeros((samples, self.timeline.output_channels), dtype=np.float32)
        contributed = False
        for track_id in sorted(self.audio):
            track = self.audio[track_id].track
            clip = self._clip_at(track_id, seconds)
            if clip is None:
                continue
            offset = self._source_offset_seconds(track, clip, seconds)
            if offset < 0:
                # negative source offset inside a clip: the source has not
                # started yet, so this track contributes silence (documented)
                contributed = True
                continue
            cursor = self._audio_cursors.get(track_id)
            if cursor is None:
                cursor = _AudioCursor(self.audio[track_id], self.timeline.output_channels)
                self._audio_cursors[track_id] = cursor
            block, padded = cursor.read(offset, samples, self.timeline.output_sample_rate)
            self.stats.tail_padded_samples += padded
            self.stats.audio_blocks += 1
            if block.shape[1] != mix.shape[1]:
                block = np.resize(block, (block.shape[0], mix.shape[1]))
            mix += block
            contributed = True
        return mix if contributed else None

    # -- driver ----------------------------------------------------------
    def frames(self) -> Iterator[RenderedFrame]:
        total = self.frame_count()
        end = self.timeline.duration().seconds()
        try:
            for number in range(self.resume_from_frame, total):
                seconds = self.frame_time(number)
                # the final frame may be partial: never emit audio past the
                # declared programme end
                duration = min(self.frame_time(number + 1), end) - seconds
                if duration <= 0:
                    break
                canvas, spans = self._video_for(seconds)
                if canvas is None:
                    canvas = self._canvas
                audio = self._audio_for(seconds, duration)
                pts_ticks = round_rational(seconds / self.timeline.time_base.seconds_per_tick, Rounding.HALF_EVEN)
                frame = RenderedFrame(
                    number=number,
                    pts_ticks=pts_ticks,
                    time_base=self.timeline.time_base,
                    array=canvas,
                    spans=tuple(spans),
                    audio=audio,
                )
                self.stats.frames += 1
                if frame.recovered:
                    self.stats.recovered_frames += 1
                if frame.concealed:
                    self.stats.concealed_frames += 1
                yield frame
        finally:
            for cursor in self._audio_cursors.values():
                cursor.close()
            for cursor in self._cursors.values():
                self.stats.video_restarts += cursor.restarts
