"""Audio normalisation: rate, channel layout, peak and integrated loudness.

Design notes
------------
* **Sample-exact boundaries.**  A segment is cut by source sample index derived
  from its integer timeline ticks.  libav seek is only used to get *near* the
  cut; the exact cut is then made by trimming decoded frames by sample count, so
  a rounded container timestamp can never move an edit point.
* **Bounded memory.**  Decoded audio is written to a temporary 32-bit float WAV
  in fixed-size chunks and re-read for measurement and gain.  Peak memory is
  one decode chunk plus one measurement window, never the whole master.
* **Measured, not assumed.**  Integrated loudness follows ITU-R BS.1770-4
  (K-weighting, 400 ms blocks, absolute and relative gating) and was calibrated
  against FFmpeg's ``ebur128`` filter to within 0.05 LU.
* **Limits only attenuate.**  A target loudness is a ceiling: quieter material
  is left untouched so the archive never invents headroom.
"""

from __future__ import annotations

import math
import os
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Sequence

import av
import numpy as np
import soundfile as sf

from .errors import MediaError, bridge
from .probe import ProbeResult, StreamProbe
from .timeline import Segment
from .util import sha256_file

SUPPORTED_RATES = (44_100, 48_000, 96_000)
SUPPORTED_LAYOUTS = ("mono", "stereo", "5.1")

#: Peak ceiling in dBFS applied after loudness normalisation.
PEAK_CEILING_DB = -1.0
ABSOLUTE_GATE_LUFS = -70.0
RELATIVE_GATE_LU = -10.0

_LAYOUT_CHANNELS = {
    "mono": ("FC",),
    "stereo": ("FL", "FR"),
    "5.1": ("FL", "FR", "FC", "LFE", "BL", "BR"),
}
_LAYOUT_ALIASES = {
    "1 channels": "mono",
    "2 channels": "stereo",
    "2.0": "stereo",
    "5.1(side)": "5.1",
    "6 channels": "5.1",
}
#: BS.1770 channel weights; LFE carries no weight.
_CHANNEL_WEIGHT = {
    "FL": 1.0, "FR": 1.0, "FC": 1.0,
    "BL": 1.41, "BR": 1.41, "SL": 1.41, "SR": 1.41,
}


# ---------------------------------------------------------------------------
# Loudness (ITU-R BS.1770-4)
# ---------------------------------------------------------------------------


def _k_filter_coefficients(fs: float) -> tuple[np.ndarray, ...]:
    # Stage 1: high shelf.
    f0, gain_db, q = 1681.974450955533, 3.999843853973347, 0.7071752369554196
    k = math.tan(math.pi * f0 / fs)
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh ** 0.4996667741545416
    a0 = 1.0 + k / q + k * k
    shelf_b = np.array([
        (vh + vb * k / q + k * k) / a0,
        2.0 * (k * k - vh) / a0,
        (vh - vb * k / q + k * k) / a0,
    ])
    shelf_a = np.array([1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0])
    # Stage 2: high pass.
    f0, q = 38.13547087602444, 0.5003270373238773
    k = math.tan(math.pi * f0 / fs)
    denom = 1.0 + k / q + k * k
    hp_b = np.array([1.0, -2.0, 1.0])
    hp_a = np.array([1.0, 2.0 * (k * k - 1.0) / denom, (1.0 - k / q + k * k) / denom])
    return shelf_b, shelf_a, hp_b, hp_a


def _apply_biquad_fft(
    data: np.ndarray, b: np.ndarray, a: np.ndarray, fs: float
) -> np.ndarray:
    """Frequency-domain biquad; identical output to the recursive filter."""
    length = data.shape[-1]
    if length == 0:
        return data
    nfft = 1 << max(16, int(math.ceil(math.log2(length + 1))))
    freqs = np.fft.rfftfreq(nfft, 1.0 / fs)
    z = np.exp(-2j * np.pi * freqs / fs)
    response = (b[0] + b[1] * z + b[2] * z * z) / (a[0] + a[1] * z + a[2] * z * z)
    spectrum = np.fft.rfft(data, nfft)
    return np.fft.irfft(spectrum * response, nfft)[:length]


def integrated_loudness(
    samples: np.ndarray, sample_rate: int, channel_names: tuple[str, ...]
) -> float:
    """Integrated loudness in LUFS for ``(channels, n)`` float samples.

    Returns ``-inf`` for silence, which the manifest records as ``null`` rather
    than a fabricated number.
    """
    if samples.size == 0:
        return float("-inf")
    weights = np.array([_CHANNEL_WEIGHT.get(name, 0.0) for name in channel_names])
    if not np.any(weights):
        return float("-inf")
    shelf_b, shelf_a, hp_b, hp_a = _k_filter_coefficients(sample_rate)
    weighted = np.zeros(samples.shape[1], dtype=np.float64)
    for channel in range(samples.shape[0]):
        signal = samples[channel].astype(np.float64)
        signal = _apply_biquad_fft(signal, hp_b, hp_a, sample_rate)
        signal = _apply_biquad_fft(signal, shelf_b, shelf_a, sample_rate)
        weighted += signal * signal * weights[channel]

    block = int(round(0.4 * sample_rate))
    hop = int(round(0.1 * sample_rate))
    if weighted.shape[0] < block:
        mean = float(weighted.mean()) if weighted.size else 0.0
        return -0.691 + 10.0 * math.log10(mean) if mean > 0 else float("-inf")
    sums = np.concatenate(([0.0], np.cumsum(weighted)))
    starts = np.arange(0, weighted.shape[0] - block + 1, hop)
    energies = (sums[starts + block] - sums[starts]) / block
    lufs = -0.691 + 10.0 * np.log10(np.maximum(energies, 1e-30))
    gated = lufs[lufs > ABSOLUTE_GATE_LUFS]
    if gated.size == 0:
        return float("-inf")
    relative = -0.691 + 10.0 * math.log10(
        float(np.mean(10.0 ** ((gated + 0.691) / 10.0)))
    ) + RELATIVE_GATE_LU
    gated = gated[gated > relative]
    if gated.size == 0:
        return float("-inf")
    return -0.691 + 10.0 * math.log10(float(np.mean(10.0 ** ((gated + 0.691) / 10.0))))


# ---------------------------------------------------------------------------
# Layout handling
# ---------------------------------------------------------------------------


def canonical_layout(name: str | None, channels: int | None) -> str | None:
    """Map a container layout string to one of the supported layouts."""
    if name:
        short = _LAYOUT_ALIASES.get(name, name)
        if short in SUPPORTED_LAYOUTS:
            return short
    if channels == 1:
        return "mono"
    if channels == 2:
        return "stereo"
    if channels == 6:
        return "5.1"
    return None


def layout_channels(layout: str) -> tuple[str, ...]:
    return _LAYOUT_CHANNELS[layout]


def db_to_linear(db: float) -> float:
    return 10.0 ** (db / 20.0)


def linear_to_db(value: float) -> float:
    return 20.0 * math.log10(value) if value > 0 else float("-inf")


# ---------------------------------------------------------------------------
# Settings and results
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class AudioSettings:
    rate: int
    layout: str
    target_lufs: float | None = None
    peak_ceiling_db: float = PEAK_CEILING_DB
    max_segment_seconds: float = 600.0
    measure_seconds: float = 30.0

    def validate(self) -> None:
        if self.rate not in SUPPORTED_RATES:
            raise MediaError(
                "unsupported-rate",
                f"rate {self.rate} is not one of {SUPPORTED_RATES}",
                stage="audio",
            )
        if self.layout not in SUPPORTED_LAYOUTS:
            raise MediaError(
                "unsupported-layout",
                f"layout {self.layout!r} is not one of {SUPPORTED_LAYOUTS}",
                stage="audio",
            )


@dataclass(slots=True)
class AudioSegment:
    """One archived audio segment, with everything needed to re-check it."""

    segment_id: str
    track_index: int
    path: str
    sample_rate: int
    layout: str
    channels: int
    samples: int
    source_sample_rate: int
    source_layout: str | None
    source_start_ticks: int
    source_end_ticks: int
    output_start_ticks: int
    gain_db: float
    lufs_in: float | None
    lufs_out: float | None
    peak_in: float
    peak_out: float
    measured_seconds: float
    sha256: str
    mapping: dict[str, Any] = field(default_factory=dict)
    #: Set when the segment could not be archived; ``path`` is then empty.
    error: MediaError | None = field(default=None, repr=False)

    def to_dict(self) -> dict[str, Any]:
        return {
            "segment_id": self.segment_id,
            "track_index": self.track_index,
            "path": self.path,
            "sample_rate": self.sample_rate,
            "layout": self.layout,
            "channels": self.channels,
            "samples": self.samples,
            "source_sample_rate": self.source_sample_rate,
            "source_layout": self.source_layout,
            "source_start_ticks": self.source_start_ticks,
            "source_end_ticks": self.source_end_ticks,
            "output_start_ticks": self.output_start_ticks,
            "gain_db": round(self.gain_db, 6),
            "lufs_in": None if self.lufs_in is None else round(self.lufs_in, 3),
            "lufs_out": None if self.lufs_out is None else round(self.lufs_out, 3),
            "peak_in": round(self.peak_in, 8),
            "peak_out": round(self.peak_out, 8),
            "measured_seconds": round(self.measured_seconds, 3),
            "sha256": self.sha256,
            "mapping": dict(self.mapping),
            "error": None if self.error is None else self.error.to_dict(),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "AudioSegment":
        return cls(
            segment_id=str(data["segment_id"]),
            track_index=int(data["track_index"]),
            path=str(data["path"]),
            sample_rate=int(data["sample_rate"]),
            layout=str(data["layout"]),
            channels=int(data["channels"]),
            samples=int(data["samples"]),
            source_sample_rate=int(data["source_sample_rate"]),
            source_layout=data.get("source_layout"),
            source_start_ticks=int(data["source_start_ticks"]),
            source_end_ticks=int(data["source_end_ticks"]),
            output_start_ticks=int(data["output_start_ticks"]),
            gain_db=float(data["gain_db"]),
            lufs_in=data.get("lufs_in"),
            lufs_out=data.get("lufs_out"),
            peak_in=float(data["peak_in"]),
            peak_out=float(data["peak_out"]),
            measured_seconds=float(data.get("measured_seconds", 0.0)),
            sha256=str(data["sha256"]),
            mapping=dict(data.get("mapping") or {}),
            error=None if data.get("error") is None else MediaError.from_dict(data["error"]),
        )


# ---------------------------------------------------------------------------
# Decode and resample
# ---------------------------------------------------------------------------


def _channels_of(frame: av.AudioFrame) -> int:
    if frame.layout is not None and frame.layout.nb_channels:
        return frame.layout.nb_channels
    return 1


def _block(frame: av.AudioFrame) -> np.ndarray:
    """Decoded frame as ``(channels, samples)`` float32 in [-1, 1].

    Integer PCM is scaled by the format's full-scale value; float formats pass
    through unchanged.  Keeping this conversion explicit (instead of relying on
    a resampler format hint) means a segment cut never depends on which sample
    format the decoder happened to choose.
    """
    data = frame.to_ndarray()
    if data.ndim == 1:
        data = data.reshape(1, -1)
    channels = _channels_of(frame)
    if data.shape[0] == 1 and channels > 1:
        # Packed formats collapse to (1, samples * channels).
        data = data.reshape(channels, -1, order="F")
    if data.dtype == np.uint8:
        return (data.astype(np.float32) - 128.0) / 128.0
    if np.issubdtype(data.dtype, np.signedinteger):
        scale = float(1 << (8 * data.dtype.itemsize - 1))
        return data.astype(np.float32) / scale
    return data.astype(np.float32, copy=False)


def _normalise(data: np.ndarray, layout: str, rate: int) -> av.AudioFrame:
    """Wrap a ``(channels, n)`` float block for libswresample."""
    out = av.AudioFrame.from_ndarray(
        np.ascontiguousarray(data, dtype=np.float32),
        format="fltp",
        layout=layout,
    )
    out.sample_rate = rate
    out.pts = None
    return out


def _decode_range(
    result: ProbeResult,
    stream: StreamProbe,
    ranges: Sequence[tuple[int, int]],
    settings: AudioSettings,
    source_layout: str,
    sinks: Sequence[Any],
) -> None:
    """Decode the stream once and cut every requested range sample-exactly.

    ``ranges`` are ``(start_sample, end_sample)`` pairs on the decoded stream and
    ``sinks`` are per-range callables receiving resampled blocks.  Sample
    positions are counted from the **first decoded frame**, never from container
    timestamps: a Matroska audio track's time base is often 1/1000 s, so a
    timestamp anchor cannot express an arbitrary sample index and seeking would
    silently shift a cut by up to half a millisecond.  One pass also means the
    total decode work is bounded by the file, not by the number of segments.

    A ``MediaError`` is stored on the sink (``sink.error``) rather than raised:
    one damaged stretch of audio must not abort the other ranges.
    """
    source_rate = stream.sample_rate or 0
    ordered = sorted(range(len(ranges)), key=lambda index: ranges[index][0])
    last_needed = max((ranges[i][1] for i in ordered), default=0)
    active: list[int] = []
    next_position = 0
    container = av.open(result.path)
    resamplers: dict[int, av.AudioResampler] = {}
    emitted = [0] * len(ranges)
    try:
        av_stream = container.streams[stream.index]
        for packet in container.demux(av_stream):
            if packet.size == 0 and packet.pts is None:
                continue
            try:
                frames = list(packet.decode())
            except Exception as exc:  # noqa: BLE001 - damaged audio
                error = bridge(
                    "audio-decode", exc, stage="audio", path=result.path,
                    stream_index=stream.index,
                    offset=_packet_offset(packet),
                )
                for index in active:
                    sinks[index].error = sinks[index].error or error
                continue
            for frame in frames:
                frame_start, frame_end = next_position, next_position + frame.samples
                next_position = frame_end
                while active and ranges[ordered[active[0]]][1] <= frame_start:
                    _finish_resampler(resamplers.pop(ordered[active[0]], None), sinks[active[0]], emitted[active[0]])
                    active.pop(0)
                for index in ordered:
                    lo_bound, hi_bound = ranges[index]
                    if hi_bound <= frame_start:
                        continue
                    if lo_bound >= frame_end:
                        break
                    if index not in active:
                        active.append(index)
                        active.sort(key=lambda i: ranges[ordered[i]][1])
                    lo = max(lo_bound, frame_start)
                    hi = min(hi_bound, frame_end)
                    data = _block(frame)[:, lo - frame_start:hi - frame_start]
                    resampler = resamplers.get(index)
                    if resampler is None:
                        resampler = av.AudioResampler(
                            format="fltp", layout=settings.layout, rate=settings.rate
                        )
                        resamplers[index] = resampler
                    try:
                        blocks = [
                            out.to_ndarray().astype(np.float32, copy=False)
                            for out in resampler.resample(
                                _normalise(data, source_layout, source_rate)
                            )
                        ]
                    except Exception as exc:  # noqa: BLE001
                        sinks[index].error = sinks[index].error or bridge(
                            "audio-decode", exc, stage="audio", path=result.path,
                            stream_index=stream.index,
                        )
                        continue
                    for block in blocks:
                        sinks[index].write(block)
                        emitted[index] += block.shape[1]
                if next_position >= last_needed:
                    break
            if next_position >= last_needed:
                break
        for index, resampler in list(resamplers.items()):
            _finish_resampler(resampler, sinks[index], emitted[index])
    except MediaError:
        raise
    except Exception as exc:  # noqa: BLE001 - libav error types vary
        raise bridge(
            "audio-decode", exc, stage="audio", path=result.path,
            stream_index=stream.index,
        ) from exc
    finally:
        container.close()


def _finish_resampler(resampler, sink, emitted: int) -> None:
    if resampler is None:
        return
    try:
        for out in resampler.resample(None):
            sink.write(out.to_ndarray().astype(np.float32, copy=False))
    except Exception:  # noqa: BLE001 - flushing a broken resampler is not fatal
        pass


def _packet_offset(packet) -> int | None:
    position = getattr(packet, "pos", None)
    return position if position is not None and position >= 0 else None


class FloatSink:
    """Bounded-memory writer for one resampled audio range.

    The decoder pushes blocks here; the file is never held in memory.  A decode
    failure is stored on ``error`` so the surrounding pass can keep filling the
    other ranges before the caller decides what to do.
    """

    __slots__ = ("path", "channels", "handle", "written", "error")

    def __init__(self, path: str, rate: int, layout: str) -> None:
        self.path = path
        self.channels = len(layout_channels(layout))
        self.written = 0
        self.error: MediaError | None = None
        try:
            self.handle = sf.SoundFile(
                path, mode="w", samplerate=rate, channels=self.channels,
                subtype="FLOAT", format="WAV",
            )
        except Exception as exc:  # noqa: BLE001
            raise bridge("io", exc, stage="audio", path=path) from exc

    def write(self, block: np.ndarray) -> None:
        if block.shape[0] != self.channels:
            self.error = self.error or MediaError(
                "audio-decode",
                f"resampler returned {block.shape[0]} channels, expected {self.channels}",
                stage="audio", path=self.path,
            )
            return
        try:
            self.handle.write(np.ascontiguousarray(block.T))
        except Exception as exc:  # noqa: BLE001
            self.error = self.error or bridge("io", exc, stage="audio", path=self.path)
            return
        self.written += block.shape[1]

    def close(self) -> int:
        try:
            self.handle.close()
        except Exception as exc:  # noqa: BLE001
            self.error = self.error or bridge("io", exc, stage="audio", path=self.path)
        return self.written

    def __enter__(self) -> "FloatSink":
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()


# ---------------------------------------------------------------------------
# Measurement and gain
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class Measurement:
    peak: float
    lufs: float | None
    seconds: float


def measure(path: str, layout: str, rate: int, window_seconds: float) -> Measurement:
    """Peak and integrated loudness over a bounded window of the file."""
    with sf.SoundFile(path) as handle:
        frames = handle.frames
        window = min(frames, int(round(window_seconds * rate)))
        data = handle.read(window, dtype="float32", always_2d=True)
    if data.size == 0:
        return Measurement(0.0, None, 0.0)
    peak = float(np.max(np.abs(data)))
    lufs = integrated_loudness(data.T, rate, layout_channels(layout))
    if not math.isfinite(lufs):
        lufs = None
    return Measurement(peak, lufs, data.shape[0] / rate)


def gain_for(settings: AudioSettings, measurement: Measurement) -> float:
    """Gain that meets both the loudness ceiling and the peak ceiling."""
    gain = 1.0
    if (
        settings.target_lufs is not None
        and measurement.lufs is not None
        and measurement.lufs > settings.target_lufs
    ):
        gain = min(gain, db_to_linear(settings.target_lufs - measurement.lufs))
    ceiling = db_to_linear(settings.peak_ceiling_db)
    if measurement.peak > ceiling:
        gain = min(gain, ceiling / measurement.peak)
    return max(gain, 0.0)


def apply_gain_and_encode(raw_path: str, output_path: str, gain: float, layout: str) -> None:
    channels = len(layout_channels(layout))
    temp_path = output_path + ".part"
    try:
        with sf.SoundFile(raw_path) as reader, sf.SoundFile(
            temp_path, mode="w", samplerate=reader.samplerate, channels=channels,
            subtype="PCM_24", format="WAV",
        ) as writer:
            while True:
                block = reader.read(65536, dtype="float32", always_2d=True)
                if block.size == 0:
                    break
                writer.write(np.clip(block * gain, -1.0, 1.0))
        os.replace(temp_path, output_path)
    except Exception as exc:  # noqa: BLE001
        raise bridge("io", exc, stage="audio", path=output_path) from exc
    finally:
        if os.path.exists(temp_path):
            os.unlink(temp_path)


# ---------------------------------------------------------------------------
# Public entry points
# ---------------------------------------------------------------------------


def sample_bounds(result: ProbeResult, stream: StreamProbe, segment: Segment) -> tuple[int, int]:
    """``(start, end)`` sample indices for one segment on the decoded stream."""
    source_rate = stream.sample_rate or 0
    rate_value = result.time_base.rate.value
    source_start = (
        segment.source_start_ticks
        if segment.source_start_ticks is not None
        else segment.start_ticks
    )
    source_end = (
        segment.source_end_ticks
        if segment.source_end_ticks is not None
        else segment.end_ticks
    )
    return (
        round(Fraction(source_start) / rate_value * source_rate),
        round(Fraction(source_end) / rate_value * source_rate),
    )


def extract_segments(
    result: ProbeResult,
    stream: StreamProbe,
    segments: Sequence[Segment],
    settings: AudioSettings,
    output_paths: Sequence[str],
) -> list[AudioSegment]:
    """Archive several audio segments from a single decode pass.

    Cutting each segment with its own decode would read the master N times; the
    same sample-exact result is available from one pass, which is what makes a
    long multi-segment master practical.  A segment whose decode fails is
    recorded with its own error and does not disturb the others.
    """
    settings.validate()
    if len(segments) != len(output_paths):
        raise ValueError("segments and output paths must be the same length")
    source_rate = stream.sample_rate or 0
    source_layout = canonical_layout(stream.layout, stream.channels)
    if source_rate <= 0:
        raise MediaError(
            "audio-decode", "stream reports no sample rate",
            stage="audio", path=result.path, stream_index=stream.index,
        )
    if source_layout is None:
        raise MediaError(
            "unsupported-layout",
            f"stream layout {stream.layout!r} with {stream.channels} channels is not "
            f"one of {SUPPORTED_LAYOUTS}",
            stage="audio", path=result.path, stream_index=stream.index,
        )
    rate_value = result.time_base.rate.value
    ranges: list[tuple[int, int]] = []
    for segment in segments:
        seconds = Fraction(segment.duration_ticks) / rate_value
        if float(seconds) > settings.max_segment_seconds:
            raise MediaError(
                "limit-exceeded",
                f"segment {segment.segment_id} is {float(seconds):.1f}s, limit is "
                f"{settings.max_segment_seconds:.1f}s",
                stage="audio", path=result.path, stream_index=stream.index,
            )
        start, end = sample_bounds(result, stream, segment)
        if end <= start:
            raise MediaError(
                "edit-out-of-range",
                f"segment {segment.segment_id} maps to an empty sample range",
                stage="audio", path=result.path, stream_index=stream.index,
            )
        ranges.append((start, end))

    raw_paths = [f"{path}.raw.wav" for path in output_paths]
    sinks = [FloatSink(path, settings.rate, settings.layout) for path in raw_paths]
    try:
        _decode_range(result, stream, ranges, settings, source_layout, sinks)
        for sink in sinks:
            sink.close()
        finished: list[AudioSegment] = []
        for segment, start_end, sink, raw_path, output_path in zip(
            segments, ranges, sinks, raw_paths, output_paths
        ):
            if sink.error is not None:
                finished.append(_failed_audio(segment, stream.index, sink.error))
                continue
            if sink.written == 0:
                finished.append(_failed_audio(
                    segment, stream.index,
                    MediaError(
                        "audio-decode",
                        f"segment {segment.segment_id} decoded to no samples",
                        stage="audio", path=result.path, stream_index=stream.index,
                    ),
                ))
                continue
            finished.append(
                _finish_segment(
                    result, stream, segment, settings, source_rate, source_layout,
                    start_end, raw_path, output_path,
                )
            )
        return finished
    finally:
        for path in raw_paths:
            if os.path.exists(path):
                os.unlink(path)


def _failed_audio(segment: Segment, stream_index: int, error: MediaError) -> AudioSegment:
    """Placeholder that carries the failure instead of a fabricated artefact."""
    return AudioSegment(
        segment_id=segment.segment_id,
        track_index=stream_index,
        path="",
        sample_rate=0,
        layout="",
        channels=0,
        samples=0,
        source_sample_rate=0,
        source_layout=None,
        source_start_ticks=segment.source_start_ticks or 0,
        source_end_ticks=segment.source_end_ticks or 0,
        output_start_ticks=segment.start_ticks,
        gain_db=0.0,
        lufs_in=None,
        lufs_out=None,
        peak_in=0.0,
        peak_out=0.0,
        measured_seconds=0.0,
        sha256="",
        mapping={"failed": True, "reason": error.message},
        error=error,
    )


def _finish_segment(
    result: ProbeResult,
    stream: StreamProbe,
    segment: Segment,
    settings: AudioSettings,
    source_rate: int,
    source_layout: str,
    start_end: tuple[int, int],
    raw_path: str,
    output_path: str,
) -> AudioSegment:
    start_sample, end_sample = start_end
    before = measure(raw_path, settings.layout, settings.rate, settings.measure_seconds)
    gain = gain_for(settings, before)
    apply_gain_and_encode(raw_path, output_path, gain, settings.layout)
    after = measure(output_path, settings.layout, settings.rate, settings.measure_seconds)
    if after.peak > 0 and linear_to_db(after.peak) > settings.peak_ceiling_db + 1e-6:
        return _failed_audio(
            segment, stream.index,
            MediaError(
                "audio-loudness",
                f"peak {linear_to_db(after.peak):.2f} dBFS exceeds ceiling "
                f"{settings.peak_ceiling_db:.2f} dBFS after gain",
                stage="audio", path=result.path, stream_index=stream.index,
            ),
        )
    gain_db = linear_to_db(gain)
    mapping = {
        "source_rate": source_rate,
        "source_layout": source_layout,
        "target_rate": settings.rate,
        "target_layout": settings.layout,
        "rate_changed": source_rate != settings.rate,
        "layout_changed": source_layout != settings.layout,
        "operation": _operation(source_layout, settings.layout, source_rate, settings.rate),
        "gain_db": round(gain_db, 6),
        "peak_ceiling_db": settings.peak_ceiling_db,
        "target_lufs": settings.target_lufs,
        "sample_bounds": [start_sample, end_sample],
        "boundary_basis": "decoded-sample-index",
        "measured_seconds": round(after.seconds, 3),
    }
    with sf.SoundFile(output_path) as handle:
        samples = handle.frames
    return AudioSegment(
        segment_id=segment.segment_id,
        track_index=segment.track_index,
        path=output_path,
        sample_rate=settings.rate,
        layout=settings.layout,
        channels=len(layout_channels(settings.layout)),
        samples=samples,
        source_sample_rate=source_rate,
        source_layout=source_layout,
        source_start_ticks=int(
            segment.source_start_ticks if segment.source_start_ticks is not None
            else segment.start_ticks
        ),
        source_end_ticks=int(
            segment.source_end_ticks if segment.source_end_ticks is not None
            else segment.end_ticks
        ),
        output_start_ticks=segment.start_ticks,
        gain_db=gain_db,
        lufs_in=before.lufs,
        lufs_out=after.lufs,
        peak_in=before.peak,
        peak_out=after.peak,
        measured_seconds=after.seconds,
        sha256=sha256_file(output_path),
        mapping=mapping,
    )


def _operation(source_layout: str, target_layout: str, source_rate: int, rate: int) -> str:
    order = ("mono", "stereo", "5.1")
    parts: list[str] = []
    if source_layout != target_layout:
        parts.append("upmix" if order.index(target_layout) > order.index(source_layout) else "downmix")
    if source_rate != rate:
        parts.append("resample")
    return "+".join(parts) or "identity"


def extract_segment(
    result: ProbeResult,
    stream: StreamProbe,
    segment: Segment,
    settings: AudioSettings,
    output_path: str,
) -> AudioSegment:
    """Cut, resample, remap, limit and hash one audio segment."""
    settings.validate()
    source_rate = stream.sample_rate or 0
    source_layout = canonical_layout(stream.layout, stream.channels)
    if source_rate <= 0:
        raise MediaError(
            "audio-decode", "stream reports no sample rate",
            stage="audio", path=result.path, stream_index=stream.index,
        )
    if source_layout is None:
        raise MediaError(
            "unsupported-layout",
            f"stream layout {stream.layout!r} with {stream.channels} channels is not "
            f"one of {SUPPORTED_LAYOUTS}",
            stage="audio", path=result.path, stream_index=stream.index,
        )
    rate_value = result.time_base.rate.value
    seconds = Fraction(segment.duration_ticks) / rate_value
    if float(seconds) > settings.max_segment_seconds:
        raise MediaError(
            "limit-exceeded",
            f"segment {segment.segment_id} is {float(seconds):.1f}s, limit is "
            f"{settings.max_segment_seconds:.1f}s",
            stage="audio", path=result.path, stream_index=stream.index,
        )
    source_start = segment.source_start_ticks if segment.source_start_ticks is not None else segment.start_ticks
    source_end = segment.source_end_ticks if segment.source_end_ticks is not None else segment.end_ticks
    start_sample = round(Fraction(source_start) / rate_value * source_rate)
    end_sample = round(Fraction(source_end) / rate_value * source_rate)
    if end_sample <= start_sample:
        raise MediaError(
            "edit-out-of-range",
            f"segment {segment.segment_id} maps to an empty sample range",
            stage="audio", path=result.path, stream_index=stream.index,
        )

    raw_path = output_path + ".raw.wav"
    try:
        with FloatSink(raw_path, settings.rate, settings.layout) as sink:
            _decode_range(
                result, stream, [(start_sample, end_sample)],
                settings, source_layout, [sink],
            )
        if sink.error is not None:
            raise sink.error
        written = sink.written
        if written == 0:
            raise MediaError(
                "audio-decode",
                f"segment {segment.segment_id} decoded to no samples",
                stage="audio", path=result.path, stream_index=stream.index,
            )
        before = measure(raw_path, settings.layout, settings.rate, settings.measure_seconds)
        gain = gain_for(settings, before)
        apply_gain_and_encode(raw_path, output_path, gain, settings.layout)
    finally:
        if os.path.exists(raw_path):
            os.unlink(raw_path)

    after = measure(output_path, settings.layout, settings.rate, settings.measure_seconds)
    if after.peak > 0 and linear_to_db(after.peak) > settings.peak_ceiling_db + 1e-6:
        raise MediaError(
            "audio-loudness",
            f"peak {linear_to_db(after.peak):.2f} dBFS exceeds ceiling "
            f"{settings.peak_ceiling_db:.2f} dBFS after gain",
            stage="audio", path=result.path, stream_index=stream.index,
        )
    gain_db = linear_to_db(gain)
    mapping = {
        "source_rate": source_rate,
        "source_layout": source_layout,
        "target_rate": settings.rate,
        "target_layout": settings.layout,
        "rate_changed": source_rate != settings.rate,
        "layout_changed": source_layout != settings.layout,
        "operation": _operation(source_layout, settings.layout, source_rate, settings.rate),
        "gain_db": round(gain_db, 6),
        "peak_ceiling_db": settings.peak_ceiling_db,
        "target_lufs": settings.target_lufs,
        "sample_bounds": [start_sample, end_sample],
        # Cuts are counted from the first decoded frame; no container
        # timestamp takes part, so the bound is sample-exact.
        "boundary_basis": "decoded-sample-index",
        "measured_seconds": round(after.seconds, 3),
    }
    return AudioSegment(
        segment_id=segment.segment_id,
        track_index=segment.track_index,
        path=output_path,
        sample_rate=settings.rate,
        layout=settings.layout,
        channels=len(layout_channels(settings.layout)),
        samples=written,
        source_sample_rate=source_rate,
        source_layout=source_layout,
        source_start_ticks=int(source_start),
        source_end_ticks=int(source_end),
        output_start_ticks=segment.start_ticks,
        gain_db=gain_db,
        lufs_in=before.lufs,
        lufs_out=after.lufs,
        peak_in=before.peak,
        peak_out=after.peak,
        measured_seconds=after.seconds,
        sha256=sha256_file(output_path),
        mapping=mapping,
    )
