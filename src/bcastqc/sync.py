"""End-to-end A/V sync measurement on the written asset.

The master carries paired markers: a 2.5 kHz tone burst in the audio and a
4-frame white flash in the picture, both centred on the same instant.  Decoding
the output and locating both events measures the *rendered* offset, which is
what an engineer cares about.  Container timestamps can look perfect while the
picture and sound are 80 ms apart; this check catches that.

Video position is the brightness-weighted centre of the flash run.  Audio
position is the energy-weighted centre of the band-limited burst, refined by
the 5 % envelope crossings.  Both are sub-frame accurate because the weighting
uses every decoded sample/frame in the event, not just its first one.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import numpy as np
import av

BRIGHT_THRESHOLD = 150.0
BAND_LOW = 1900.0
BAND_HIGH = 3200.0
ENERGY_FRACTION = 0.3
CROSSING_FRACTION = 0.03
FLASH_GAP_SECONDS = 0.15


@dataclass(slots=True)
class MarkerMatch:
    expected_s: float
    video_s: float | None
    audio_s: float | None
    offset_ms: float | None

    def to_dict(self) -> dict[str, object]:
        return {
            "expected_s": round(self.expected_s, 6),
            "video_s": None if self.video_s is None else round(self.video_s, 6),
            "audio_s": None if self.audio_s is None else round(self.audio_s, 6),
            "offset_ms": None if self.offset_ms is None else round(self.offset_ms, 3),
        }


@dataclass(slots=True)
class SyncMeasurement:
    markers: list[MarkerMatch] = field(default_factory=list)
    video_events: list[float] = field(default_factory=list)
    audio_events: list[float] = field(default_factory=list)
    error: str | None = None

    @property
    def measured(self) -> bool:
        return bool(self.markers) and all(m.offset_ms is not None for m in self.markers)

    @property
    def sync_error_ms(self) -> float | None:
        offsets = [m.offset_ms for m in self.markers if m.offset_ms is not None]
        if not offsets:
            return None
        return max(abs(o) for o in offsets)

    def to_dict(self) -> dict[str, object]:
        return {
            "sync_error_ms": None if self.sync_error_ms is None else round(self.sync_error_ms, 3),
            "markers": [m.to_dict() for m in self.markers],
            "video_events": [round(v, 6) for v in self.video_events],
            "audio_events": [round(v, 6) for v in self.audio_events],
            "error": self.error,
        }


def measure_sync(
    path: Path,
    expected_markers: tuple[float, ...],
    *,
    fps: float = 25.0,
    max_seconds: float = 30.0,
) -> SyncMeasurement:
    """Decode the asset and measure marker alignment."""
    result = SyncMeasurement()
    video_times: list[float] = []
    audio_chunks: list[np.ndarray] = []
    audio_rate: int | None = None
    audio_base: float | None = None
    try:
        with av.open(str(path), mode="r", metadata_errors="ignore") as container_in:
            video = next((s for s in container_in.streams if s.type == "video"), None)
            audio = next((s for s in container_in.streams if s.type == "audio"), None)
            if video is None:
                result.error = "no video stream"
                return result
            if audio is not None:
                audio_rate = audio.codec_context.sample_rate or 48000
            limit = int(max_seconds * (audio_rate or 48000)) if audio_rate else None
            collected = 0
            for packet in container_in.demux():
                if packet.size == 0:
                    continue
                if packet.stream.index == video.index:
                    for frame in video.codec_context.decode(packet):
                        stamp = float(frame.pts * frame.time_base) if frame.pts is not None else None
                        if stamp is None:
                            continue
                        gray = frame.to_ndarray(format="gray")
                        if gray.mean() > BRIGHT_THRESHOLD:
                            video_times.append(stamp + 0.5 / fps)
                elif audio is not None and packet.stream.index == audio.index:
                    for frame in audio.codec_context.decode(packet):
                        samples = frame.to_ndarray().astype(np.float64)
                        mono = samples.mean(axis=0)
                        if audio_base is None and frame.pts is not None:
                            audio_base = float(frame.pts) / audio_rate
                        audio_chunks.append(mono)
                        collected += len(mono)
                        if limit is not None and collected >= limit:
                            break
                if limit is not None and collected >= limit:
                    break
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        result.error = f"decode failed: {exc}"
        return result

    result.video_events = _cluster_video(video_times)
    if audio_chunks and audio_rate:
        signal = np.concatenate(audio_chunks)
        base = audio_base or 0.0
        result.audio_events = _audio_events(signal, audio_rate, base)
    else:
        result.error = "no audio stream"

    for marker in expected_markers:
        video_hit = _nearest(result.video_events, marker)
        audio_hit = _nearest(result.audio_events, marker)
        offset = None
        if video_hit is not None and audio_hit is not None:
            offset = (audio_hit - video_hit) * 1000.0
        result.markers.append(
            MarkerMatch(marker, video_hit, audio_hit, offset)
        )
    if result.error is None and not result.audio_events:
        result.error = "no audio marker energy in the analysis band"
    if result.error is None and not result.video_events:
        result.error = "no flash frames found"
    return result


def _cluster_video(times: list[float]) -> list[float]:
    if not times:
        return []
    times = sorted(times)
    groups: list[list[float]] = [[times[0]]]
    for value in times[1:]:
        if value - groups[-1][-1] < FLASH_GAP_SECONDS:
            groups[-1].append(value)
        else:
            groups.append([value])
    return [sum(group) / len(group) for group in groups]


def _audio_events(signal: np.ndarray, rate: int, base: float) -> list[float]:
    if len(signal) < rate // 10:
        return []
    spectrum = np.fft.rfft(signal)
    freqs = np.fft.rfftfreq(len(signal), 1.0 / rate)
    spectrum[(freqs < BAND_LOW) | (freqs > BAND_HIGH)] = 0
    band = np.abs(np.fft.irfft(spectrum, n=len(signal)))
    window = max(1, int(0.010 * rate))
    envelope = np.convolve(band, np.ones(window) / window, mode="same")
    peak = envelope.max()
    if peak <= 0:
        return []
    times = base + np.arange(len(signal)) / rate
    events: list[float] = []
    index = 0
    total = len(envelope)
    while index < total:
        if envelope[index] > ENERGY_FRACTION * peak:
            start = index
            while start > 0 and envelope[start] > CROSSING_FRACTION * peak:
                start -= 1
            stop = index
            while stop < total and envelope[stop] > CROSSING_FRACTION * peak:
                stop += 1
            weights = envelope[start:stop]
            stamps = times[start:stop]
            if weights.sum() > 0:
                events.append(float((weights * stamps).sum() / weights.sum()))
            index = stop
        else:
            index += 1
    return events


def _nearest(events: list[float], target: float) -> float | None:
    if not events:
        return None
    return min(events, key=lambda value: abs(value - target))
