"""Chunked PCM/WAV measurement and A/V timeline alignment.

Audio is never loaded whole: :func:`analyze_wav` streams fixed-size blocks
through ``soundfile.blocks`` and only keeps scalar accumulators plus a bounded
list of silence intervals.  Peak/RMS values are exact for the whole file even
when a block boundary falls inside a silence run, because the silence detector
carries its state across blocks instead of judging each block in isolation.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .clock import align_audio_to_video
from .errors import MediaInputError

#: Window used to measure the noise floor when deriving a silence threshold.
DEFAULT_SILENCE_DB = -50.0
DEFAULT_MIN_SILENCE_S = 0.05
MAX_SILENCE_INTERVALS = 4096


@dataclass
class SilenceInterval:
    start_s: float
    end_s: float

    @property
    def duration_s(self) -> float:
        return self.end_s - self.start_s

    def to_dict(self) -> dict[str, Any]:
        return {
            "start_s": round(self.start_s, 6),
            "end_s": round(self.end_s, 6),
            "duration_s": round(self.duration_s, 6),
        }


@dataclass
class AudioAnalysis:
    path: str
    sample_rate: int
    channels: int
    frames: int
    duration_s: float
    peak: float
    peak_dbfs: float
    rms: float
    rms_dbfs: float
    silence_db: float
    silence_intervals: list[SilenceInterval] = field(default_factory=list)
    silence_truncated: bool = False
    blocks: int = 0
    block_frames: int = 0
    block_starts: list[float] = field(default_factory=list)
    channel_layout: str = ""

    @property
    def silent_fraction(self) -> float:
        if not self.duration_s:
            return 0.0
        return sum(i.duration_s for i in self.silence_intervals) / self.duration_s

    def block_starts_fraction(self) -> list[Fraction]:
        return [Fraction(s).limit_denominator(self.sample_rate) for s in
                (self.block_starts or [0.0])]

    def to_dict(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "channel_layout": self.channel_layout,
            "frames": self.frames,
            "duration_s": round(self.duration_s, 6),
            "peak": round(self.peak, 6),
            "peak_dbfs": round(self.peak_dbfs, 3),
            "rms": round(self.rms, 6),
            "rms_dbfs": round(self.rms_dbfs, 3),
            "silence_db": self.silence_db,
            "silence_intervals": [i.to_dict() for i in self.silence_intervals],
            "silence_truncated": self.silence_truncated,
            "silent_fraction": round(self.silent_fraction, 4),
            "blocks": self.blocks,
            "block_frames": self.block_frames,
        }


def _dbfs(value: float) -> float:
    if value <= 0.0:
        return -240.0
    return float(20.0 * np.log10(value))


def analyze_wav(
    path: str | Path,
    *,
    block_frames: int = 4096,
    silence_db: float = DEFAULT_SILENCE_DB,
    min_silence_s: float = DEFAULT_MIN_SILENCE_S,
    collect_block_starts: int = 0,
    block_iter=None,
) -> AudioAnalysis:
    """Measure a WAV file in bounded blocks.

    Peak and RMS are exact whole-file values.  Silent runs are detected on the
    sample stream with the threshold applied per block, while the run itself is
    tracked across block boundaries.  ``collect_block_starts`` limits how many
    block start times are retained for alignment (0 keeps them all, which is
    still only one float per block, not per sample).
    """
    path = Path(path)
    if not path.exists():
        raise MediaInputError(f"audio file not found: {path}", code="audio_missing")
    if block_frames <= 0:
        raise MediaInputError("block_frames must be positive", code="audio_bad_block")
    try:
        info = sf.info(str(path))
    except RuntimeError as exc:
        raise MediaInputError(f"cannot read audio header: {exc}", code="audio_header") from exc
    if info.frames <= 0:
        raise MediaInputError("audio file has no frames", code="audio_empty")

    peak = 0.0
    sum_squares = 0.0
    total = 0
    threshold = 10.0 ** (silence_db / 20.0)
    min_silence_frames = int(round(min_silence_s * info.samplerate))
    silences: list[SilenceInterval] = []
    truncated = False

    run_start: int | None = None  # absolute frame index where silence began
    prev_quiet = False
    previous_end = 0
    blocks = 0
    starts: list[float] = []

    def close_silence(end_frame: int) -> None:
        nonlocal run_start, truncated
        if run_start is None:
            return
        if end_frame - run_start >= min_silence_frames and end_frame > run_start:
            if len(silences) < MAX_SILENCE_INTERVALS:
                silences.append(SilenceInterval(
                    run_start / info.samplerate, end_frame / info.samplerate))
            else:
                truncated = True
        run_start = None

    if block_iter is None:
        block_iter = sf.blocks(str(path), blocksize=block_frames, dtype="float64",
                               always_2d=True, fill_value=0.0)
    try:
        for block in block_iter:
            if block.size == 0:
                continue
            # soundfile pads the final block to full size when fill_value is
            # given; trim to the real frame count so padding is neither measured
            # nor mistaken for silence.
            remaining = info.frames - previous_end
            if remaining <= 0:
                break
            block = block[:remaining]
            blocks += 1
            if collect_block_starts <= 0 or len(starts) < collect_block_starts:
                starts.append(previous_end / info.samplerate)
            block_start = previous_end
            previous_end += len(block)
            peak = max(peak, float(np.max(np.abs(block))))
            sum_squares += float(np.sum(np.square(block, dtype=np.float64)))
            total += int(block.size)

            # Per-frame magnitude, judged against the threshold.  Transitions are
            # detected against the previous block's final state so a silence run
            # spanning a block boundary is measured as one interval.
            frame_mag = np.max(np.abs(block), axis=1)
            quiet = frame_mag <= threshold
            states = np.concatenate(([prev_quiet], quiet))
            changes = np.flatnonzero(states[1:] != states[:-1])
            for change in changes:
                if quiet[change]:  # became quiet at this frame
                    run_start = block_start + int(change)
                else:  # became loud again
                    close_silence(block_start + int(change))
            prev_quiet = bool(quiet[-1])
        if prev_quiet:
            close_silence(previous_end)
    except RuntimeError as exc:
        raise MediaInputError(f"cannot read audio data: {exc}", code="audio_read") from exc

    if total <= 0:
        raise MediaInputError("audio file has no samples", code="audio_empty")
    rms = float(np.sqrt(sum_squares / total))
    duration = total / info.samplerate
    return AudioAnalysis(
        path=str(path), sample_rate=info.samplerate, channels=info.channels,
        frames=total, duration_s=duration,
        peak=peak, peak_dbfs=_dbfs(peak),
        rms=rms, rms_dbfs=_dbfs(rms),
        silence_db=silence_db, silence_intervals=silences,
        silence_truncated=truncated, blocks=blocks, block_frames=block_frames,
        block_starts=starts, channel_layout=_layout(info.channels))


def _layout(channels: int) -> str:
    return {1: "mono", 2: "stereo"}.get(channels, f"{channels}-channel")


def align_audio(
    analysis: AudioAnalysis,
    video_frame_times_s: list[Fraction] | list[float],
    *,
    tolerance_ms: float = 40.0,
) -> dict[str, Any]:
    """Align audio block starts against decoded video frame times."""
    video = [Fraction(x).limit_denominator(1_000_000) for x in video_frame_times_s]
    audio = [Fraction(x).limit_denominator(analysis.sample_rate)
             for x in analysis.block_starts]
    result = align_audio_to_video(audio, video, sample_rate=analysis.sample_rate,
                                  tolerance_ms=tolerance_ms)
    result["audio_duration_s"] = analysis.duration_s
    if video:
        result["video_duration_s"] = float(video[-1] - video[0])
    # Block starts (not per-sample times) are compared: a start that happens to
    # fall between two frame boundaries is snapped by at most half a block.
    result["within_tolerance"] = bool(result.get("max_alignment_ms", 0.0) <= tolerance_ms)
    result["block_ms"] = 1000.0 * analysis.block_frames / analysis.sample_rate
    result["tolerance_ms"] = tolerance_ms
    return result
