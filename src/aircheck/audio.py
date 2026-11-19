"""Downsampled audio inspection.

Decoding a full broadcast asset to float is wasteful when the operator only
needs channel count, peak level and silent interval boundaries.  This module
decodes in bounded chunks and *samples* frames (every Nth), so memory stays flat
and the cost is proportional to the sample budget rather than to duration.

What it reports per audio stream:

* channel count as seen on the first decoded frame (authoritative: it comes from
  the codec context, not the container metadata, which can disagree);
* peak amplitude as linear full-scale (``0.0`` silent, ``1.0`` full scale);
* silent intervals longer than ``min_silence_seconds``;
* sampling anomalies: sample rates outside the plausible broadcast set, channel
  count changes mid-stream, and decode failures (which are collected, never
  swallowed).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

import av
import numpy as np

from .probe import ProbeResult, StreamInfo
from .timebase import DecodeError

#: Sample rates a master-control ingest would accept for programme audio.
PLAUSIBLE_SAMPLE_RATES = {32000, 44100, 48000, 88200, 96000, 176400, 192000}

#: Linear amplitude below which a frame counts as silence.
DEFAULT_SILENCE_THRESHOLD = 1e-3


@dataclass(frozen=True)
class SilenceInterval:
    start_ticks: int
    end_ticks: int

    @property
    def duration_ticks(self) -> int:
        return self.end_ticks - self.start_ticks

    def to_dict(self) -> dict[str, object]:
        return {
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "duration_ticks": self.duration_ticks,
        }


@dataclass
class AudioAnomaly:
    kind: str
    detail: str

    def to_dict(self) -> dict[str, str]:
        return {"kind": self.kind, "detail": self.detail}


@dataclass
class AudioStreamReport:
    stream_index: int
    channels: int | None = None
    sample_rate: int | None = None
    frames_sampled: int = 0
    frames_decoded: int = 0
    peak: float = 0.0
    rms: float = 0.0
    silent_seconds: float = 0.0
    silence: list[SilenceInterval] = field(default_factory=list)
    anomalies: list[AudioAnomaly] = field(default_factory=list)
    decode_failures: int = 0

    @property
    def silent_ratio(self) -> float:
        if not self.frames_decoded:
            return 0.0
        return self.silent_seconds / max(self.silent_seconds + 1e-9, self.silent_seconds + 1e-9)

    def to_dict(self) -> dict[str, object]:
        return {
            "stream_index": self.stream_index,
            "channels": self.channels,
            "sample_rate": self.sample_rate,
            "frames_sampled": self.frames_sampled,
            "frames_decoded": self.frames_decoded,
            "peak": round(self.peak, 6),
            "rms": round(self.rms, 6),
            "silent_seconds": round(self.silent_seconds, 4),
            "silence": [interval.to_dict() for interval in self.silence],
            "anomalies": [anomaly.to_dict() for anomaly in self.anomalies],
            "decode_failures": self.decode_failures,
        }


@dataclass
class AudioReport:
    streams: list[AudioStreamReport] = field(default_factory=list)
    sampled: bool = True
    sample_stride: int = 1

    @property
    def ok(self) -> bool:
        return all(not stream.decode_failures for stream in self.streams)

    def to_dict(self) -> dict[str, object]:
        return {
            "sampled": self.sampled,
            "sample_stride": self.sample_stride,
            "streams": [stream.to_dict() for stream in self.streams],
            "ok": self.ok,
        }


def _to_float(frame: av.AudioFrame, channels: int) -> np.ndarray:
    """Return frame samples as float32 in [-1, 1], shape ``(channels, samples)``.

    PyAV exposes planar formats as ``(channels, samples)`` and packed formats as
    ``(1, samples * channels)``.  Packed data is de-interleaved here so callers
    always see one row per channel.  Integer formats are scaled by their own
    full-scale value, so 16- and 24-bit audio map to the same [-1, 1] range.
    """
    array = frame.to_ndarray()
    if array.ndim == 1:
        array = array.reshape(1, -1)
    if array.shape[0] == 1 and channels > 1:
        # Packed: one interleaved row -> (channels, samples).
        array = array.reshape(-1, channels).T
    if not np.issubdtype(array.dtype, np.floating):
        info = np.iinfo(array.dtype)
        scale = float(max(abs(info.min), info.max))
        return array.astype(np.float32) / scale
    return array.astype(np.float32, copy=False)


def _ticks(frame: av.AudioFrame, stream: StreamInfo) -> tuple[int | None, int | None]:
    if frame.pts is None:
        return None, None
    time_base = stream.time_base
    start = (frame.pts * time_base.numerator * 1_000_000_000) // time_base.denominator
    if frame.sample_rate:
        duration = (frame.samples * 1_000_000_000) // frame.sample_rate
    else:
        duration = 0
    return start, start + duration


def inspect_audio(
    path: str | Path,
    probe_result: ProbeResult,
    *,
    sample_stride: int = 8,
    silence_threshold: float = DEFAULT_SILENCE_THRESHOLD,
    min_silence_seconds: float = 0.05,
    max_chunks: int | None = None,
) -> AudioReport:
    """Sample every audio stream and summarise level and silence.

    ``sample_stride`` decodes one chunk in every ``stride``; ``stride=1``
    inspects everything.  ``max_chunks`` caps work for very long assets.
    """
    source = Path(path)
    streams = probe_result.audio_streams
    if not streams:
        return AudioReport(streams=[], sampled=sample_stride > 1, sample_stride=sample_stride)
    if sample_stride < 1:
        raise ValueError("sample_stride must be at least 1")

    try:
        container = av.open(str(source))
    except av.error.FFmpegError as exc:
        raise DecodeError(
            "audio inspection could not open the container",
            path=str(source),
            error=str(exc),
        ) from exc

    reports = {stream.index: AudioStreamReport(stream_index=stream.index) for stream in streams}
    by_index = {stream.index: stream for stream in streams}
    chunk_seen = {stream.index: 0 for stream in streams}
    silence_open: dict[int, int] = {}
    report_last_end: dict[int, int] = {}
    active_channels: dict[int, int | None] = {stream.index: None for stream in streams}
    rms_accum = {stream.index: 0.0 for stream in streams}
    rms_frames = {stream.index: 0 for stream in streams}
    try:
        for packet in container.demux():
            if packet.size == 0:
                continue
            stream = by_index.get(packet.stream.index)
            if stream is None:
                continue
            index = stream.index
            try:
                frames = packet.decode()
            except av.error.FFmpegError as exc:
                reports[index].decode_failures += 1
                reports[index].anomalies.append(
                    AudioAnomaly("decode_failure", f"decoder raised at pts={packet.pts}: {exc}")
                )
                continue
            for frame in frames:
                should_sample = chunk_seen[index] % sample_stride == 0
                chunk_seen[index] += 1
                if max_chunks is not None and chunk_seen[index] > max_chunks * sample_stride:
                    continue
                if not should_sample:
                    continue
                _consume_frame(
                    frame, stream, reports[index], silence_open, rms_accum, rms_frames,
                    silence_threshold, min_silence_seconds, active_channels,
                    report_last_end,
                )
        # Close any silence runs still open at end of stream.
        for index, start in list(silence_open.items()):
            report = reports[index]
            end = report_last_end.get(index, start)
            _close_silence(report, start, end, min_silence_seconds)
            silence_open.pop(index, None)
    finally:
        container.close()

    for index, report in reports.items():
        if rms_frames[index]:
            report.rms = float(np.sqrt(rms_accum[index] / rms_frames[index]))
    return AudioReport(
        streams=[reports[stream.index] for stream in streams],
        sampled=sample_stride > 1,
        sample_stride=sample_stride,
    )


def _consume_frame(
    frame: av.AudioFrame,
    stream: StreamInfo,
    report: AudioStreamReport,
    silence_open: dict[int, int | None],
    rms_accum: dict[int, float],
    rms_frames: dict[int, int],
    silence_threshold: float,
    min_silence_seconds: float,
    expected_channels: dict[int, int | None],
    last_end: dict[int, int],
) -> None:
    index = stream.index
    channels = len(frame.layout.channels) if frame.layout else None
    if report.channels is None:
        report.channels = channels
        report.sample_rate = frame.sample_rate or stream.sample_rate
        if report.sample_rate not in PLAUSIBLE_SAMPLE_RATES and report.sample_rate:
            report.anomalies.append(
                AudioAnomaly(
                    "unusual_sample_rate",
                    f"{report.sample_rate} Hz is outside the accepted broadcast set",
                )
            )
        expected_channels[index] = channels
    elif channels is not None and expected_channels[index] is not None and channels != expected_channels[index]:
        report.anomalies.append(
            AudioAnomaly(
                "channel_count_changed",
                f"channel count changed from {expected_channels[index]} to {channels}",
            )
        )
        expected_channels[index] = channels

    samples = _to_float(frame, channels or 1)
    report.frames_decoded += 1
    if frame.sample_rate:
        report.frames_sampled += 1
    if samples.size == 0:
        return
    peak = float(np.max(np.abs(samples)))
    report.peak = max(report.peak, peak)
    rms_accum[index] += float(np.mean(np.square(samples))) * samples.size
    rms_frames[index] += samples.size

    start_ticks, end_ticks = _ticks(frame, stream)
    seconds = (end_ticks - start_ticks) / 1_000_000_000 if start_ticks is not None else 0.0
    is_silent = peak < silence_threshold
    if is_silent:
        report.silent_seconds += seconds
    if start_ticks is None or end_ticks is None:
        return
    last_end[index] = end_ticks
    current_start = silence_open.get(index)
    if is_silent:
        if current_start is None:
            silence_open[index] = start_ticks
    elif current_start is not None:
        _close_silence(report, current_start, start_ticks, min_silence_seconds)
        silence_open[index] = None


def _close_silence(
    report: AudioStreamReport,
    start_ticks: int,
    end_ticks: int,
    min_silence_seconds: float,
) -> None:
    interval = SilenceInterval(start_ticks=start_ticks, end_ticks=end_ticks)
    if interval.duration_ticks >= min_silence_seconds * 1_000_000_000:
        report.silence.append(interval)


__all__ = [
    "AudioAnomaly",
    "AudioReport",
    "AudioStreamReport",
    "DEFAULT_SILENCE_THRESHOLD",
    "PLAUSIBLE_SAMPLE_RATES",
    "SilenceInterval",
    "inspect_audio",
]
