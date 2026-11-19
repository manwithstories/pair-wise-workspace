"""Unified-timeline construction: clock mapping, VFR frames and audio offsets.

Video frame timing is taken from packet capture timestamps (sample time), not
an assumed fixed frame rate, so variable frame rates are handled natively.
All boundary math stays in integer 90 kHz ticks (or exact Fractions); only
the final reporting step converts to milliseconds.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from fractions import Fraction

import numpy as np

from .clock import (
    TICKS_PER_MS,
    TICKS_PER_SECOND,
    DriftModel,
    DriftSegment,
    _fit_line,
)
from .errors import ErrorLog, Recovery, Stage
from .packets import PacketStream

# uncertainty guardrails (ticks)
MIN_SANE_DELTA = 1  # non-positive deltas are ambiguous
MAX_DELTA_FACTOR = 3  # >3x median interval -> uncertain interval


@dataclass
class Frame:
    index: int
    capture_ts: int       # unwrapped capture-clock ticks
    ref_ts: int           # unified-timeline ticks (rounded integer)
    stream_offset: int    # first byte of the access unit in reassembled stream
    is_keyframe: bool
    uncertain: bool = False


@dataclass
class VideoTimeline:
    track: str
    codec: str
    frames: list[Frame] = field(default_factory=list)
    drift: DriftModel | None = None
    segment_breaks: list[int] = field(default_factory=list)
    stream_path: str = ""
    stream_size: int = 0

    @property
    def duration_ticks(self) -> int:
        return self.frames[-1].ref_ts - self.frames[0].ref_ts if self.frames else 0


@dataclass
class AudioTimeline:
    track: str
    path: str
    samplerate: int
    frames: int
    measured_offset_ticks: int = 0
    expected_offset_ticks: int = 0
    overrun_ticks: int = 0  # audio extending past the video timeline (not clipped)

    @property
    def duration_ticks(self) -> int:
        return round(self.frames * TICKS_PER_SECOND / self.samplerate)


def reassemble(stream: PacketStream, out_path: str) -> list[tuple[int, int, int, int]]:
    """Write packet payloads back to a byte stream.

    Returns per-packet (seq, start_offset, end_offset, unwrapped_ts) so frame
    timestamps can be mapped to byte positions without rescanning packets.
    """
    spans: list[tuple[int, int, int, int]] = []
    offset = 0
    with open(out_path, "wb") as f:
        for p in stream.packets:
            f.write(p.payload)
            spans.append((p.seq, offset, offset + len(p.payload), p.ts_unwrapped))
            offset += len(p.payload)
    return spans


def _span_for_offset(spans: list[tuple[int, int, int, int]], pos: int) -> tuple | None:
    for span in spans:
        if span[1] <= pos < span[2]:
            return span
    return None


def build_video_timeline(
    track: str,
    codec: str,
    stream: PacketStream,
    units: list,
    spans: list[tuple[int, int, int, int]],
    stream_path: str,
    stream_size: int,
    log: ErrorLog,
) -> VideoTimeline:
    """Build frame list from access-unit units and packet sample times."""
    tl = VideoTimeline(track=track, codec=codec, stream_path=stream_path,
                       stream_size=stream_size)
    # One packet carries one encoded access unit; attribute each key unit and
    # count VCL-bearing units as frames via their containing packet span.
    frame_packets: list[tuple] = []
    used_seqs: set[int] = set()
    for u in units:
        if not u.complete:
            continue
        span = _span_for_offset(spans, u.offset)
        if span is None or span[0] in used_seqs:
            continue
        used_seqs.add(span[0])
        frame_packets.append(span)
    frame_packets.sort(key=lambda s: (s[3], s[0]))

    capture = [s[3] for s in frame_packets]
    model = None
    median_delta = 0
    if len(capture) >= 2:
        deltas = [b - a for a, b in zip(capture, capture[1:])]
        median_delta = int(np.median(deltas)) if deltas else 0
        # A capture-clock step shows up as a frame interval that deviates from
        # the running cadence by far more than VFR jitter. Detect it directly
        # on the capture axis, then fit one drift line per resulting segment
        # against a uniform reference clock.
        step_ticks = 10 * TICKS_PER_MS
        breaks = [i + 1 for i, d in enumerate(deltas)
                  if abs(d - median_delta) > step_ticks]
        boundaries = [0] + breaks + [len(capture)]
        reference = [capture[0] + median_delta * i
                     for i in range(len(capture))]
        tl.drift = DriftModel()
        for lo, hi in zip(boundaries, boundaries[1:]):
            slope, icept = _fit_line(capture[lo:hi], reference[lo:hi])
            tl.drift.segments.append(DriftSegment(slope, icept, lo, hi))
        tl.segment_breaks = breaks
        model = tl.drift
        for b in breaks:
            log.add(Stage.TRANSFORM, track,
                    f"capture-clock step detected at frame {b}; "
                    "new drift segment",
                    recovery=Recovery.SEGMENTED)

    key_offsets = {u.offset for u in units if u.is_keyframe and u.complete}
    prev_ts = None
    for i, span in enumerate(frame_packets):
        seq, start, _end, cap_ts = span
        if model is not None:
            ref = int(model.map(cap_ts, i))  # exact Fraction, rounded once
        else:
            ref = cap_ts
        uncertain = False
        if prev_ts is not None:
            d = cap_ts - prev_ts
            if d < MIN_SANE_DELTA or (median_delta and d > MAX_DELTA_FACTOR * median_delta):
                uncertain = True
                log.add(Stage.TRANSFORM, track,
                        f"ambiguous timestamp interval at frame {i} "
                        f"(delta_ticks={d})", byte_offset=start)
        tl.frames.append(Frame(i, cap_ts, ref, start, start in key_offsets, uncertain))
        prev_ts = cap_ts
    return tl


def estimate_audio_offset(
    samples: np.ndarray,
    reference: np.ndarray,
    samplerate: int,
    max_lag_s: float = 2.0,
) -> int:
    """Estimate ``samples`` relative offset vs a reference track, in ticks.

    Positive offset means ``samples`` starts later than the reference.
    Normalized FFT cross-correlation on the mono mix; integer-sample lag.
    """
    mono = samples.mean(axis=1) if samples.ndim > 1 else samples
    ref = reference.mean(axis=1) if reference.ndim > 1 else reference
    mono = mono - mono.mean()
    ref = ref - ref.mean()
    n = len(mono) + len(ref) - 1
    nfft = 1 << (n - 1).bit_length()
    corr = np.fft.irfft(np.fft.rfft(mono, nfft) * np.conj(np.fft.rfft(ref, nfft)))
    corr = np.concatenate((corr[-(len(ref) - 1):], corr[: len(mono)]))
    max_lag = min(int(max_lag_s * samplerate), len(corr) // 2)
    lags = np.arange(-max_lag, max_lag + 1)
    center = len(ref) - 1
    peak = lags[np.argmax(corr[center + lags])]
    return round(peak * TICKS_PER_SECOND / samplerate)


def read_audio(path: str) -> tuple[np.ndarray, int, int]:
    import soundfile as sf

    with sf.SoundFile(path) as f:
        data = f.read(always_2d=True)
        return data, f.samplerate, len(data)


def build_audio_timeline(
    track: str,
    stream: PacketStream,
    wav_path: str,
    ref_audio: np.ndarray | None,
    expected_offset_ticks: int,
    video_duration_ticks: int,
) -> AudioTimeline:
    reassemble(stream, wav_path)
    data, sr, nframes = read_audio(wav_path)
    measured = 0
    if ref_audio is not None:
        measured = estimate_audio_offset(data, ref_audio, sr)
    dur = round(nframes * TICKS_PER_SECOND / sr)
    overrun = max(0, dur + measured - video_duration_ticks)
    return AudioTimeline(track, wav_path, sr, nframes, measured,
                         expected_offset_ticks, overrun)
