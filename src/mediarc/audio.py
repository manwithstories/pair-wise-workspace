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
from typing import Any, Iterator

import av
import numpy as np
import soundfile as sf

from .errors import MediaError, bridge
from .probe import ProbeResult, StreamProbe
from .timeline import Segment
from .util import iter_packets, sha256_file

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


def _frame_pts_samples(frame: av.AudioFrame, stream: StreamProbe, rate: int) -> int | None:
    if frame.pts is None:
        return None
    return round(Fraction(frame.pts) * frame.time_base * rate) if frame.time_base else None


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


#: Below this distance libav seeking costs more precision than it saves, so the
#: range is decoded from the first sample and positions stay bit-exact.
REWIND_WINDOW_SECONDS = 8.0


def _decode_range(
    result: ProbeResult,
    stream: StreamProbe,
    start_sample: int,
    end_sample: int,
    settings: AudioSettings,
    source_layout: str,
) -> Iterator[np.ndarray]:
    """Yield resampled ``(channels, n)`` blocks for exactly the requested range.

    ``start_sample``/``end_sample`` are indices on the *decoded* stream.  When
    the range begins close to the origin the stream is decoded from sample 0, so
    the index is exact; otherwise the demuxer is seeked to a point safely before
    the cut and the first decoded frame's timestamp becomes the anchor.  The
    anchor and the resolved bounds are written into the manifest, so a verifier
    can tell which of the two paths produced the segment.
    """
    source_rate = stream.sample_rate or 0
    container = av.open(result.path)
    resampler = av.AudioResampler(
        format="fltp", layout=settings.layout, rate=settings.rate
    )
    try:
        av_stream = container.streams[stream.index]
        decode_from_zero = start_sample <= int(REWIND_WINDOW_SECONDS * source_rate)
        target = (
            Fraction(0)
            if decode_from_zero
            else Fraction(max(0, start_sample - int(2.0 * source_rate))) / source_rate
        )
        anchor: int | None = None if decode_from_zero else None
        position: int | None = 0 if decode_from_zero else None
        consumed = 0
        wanted = end_sample - start_sample
        for packet in iter_packets(container, av_stream, target):
            for frame in packet.decode():
                if position is None:
                    pts_position = _frame_pts_samples(frame, stream, source_rate)
                    position = pts_position if pts_position is not None else start_sample
                    anchor = position
                frame_end = position + frame.samples
                lo = max(position, start_sample)
                hi = min(frame_end, end_sample)
                if hi > lo:
                    data = _block(frame)[:, lo - position: hi - position]
                    consumed += data.shape[1]
                    for out in resampler.resample(
                        _normalise(data, source_layout, source_rate)
                    ):
                        yield out.to_ndarray().astype(np.float32, copy=False)
                position = frame_end
                if position >= end_sample or consumed >= wanted:
                    break
            if position is not None and (position >= end_sample or consumed >= wanted):
                break
        for out in resampler.resample(None):
            yield out.to_ndarray().astype(np.float32, copy=False)
    except MediaError:
        raise
    except Exception as exc:  # noqa: BLE001 - libav error types vary
        raise bridge(
            "audio-decode", exc, stage="audio", path=result.path,
            stream_index=stream.index,
        ) from exc
    finally:
        container.close()


def _write_float(path: str, blocks: Iterator[np.ndarray], rate: int, layout: str) -> int:
    channels = len(layout_channels(layout))
    written = 0
    try:
        with sf.SoundFile(
            path, mode="w", samplerate=rate, channels=channels,
            subtype="FLOAT", format="WAV",
        ) as handle:
            for block in blocks:
                if block.shape[0] != channels:
                    raise MediaError(
                        "audio-decode",
                        f"resampler returned {block.shape[0]} channels, expected {channels}",
                        stage="audio",
                    )
                handle.write(np.ascontiguousarray(block.T))
                written += block.shape[1]
    except MediaError:
        raise
    except Exception as exc:  # noqa: BLE001
        raise bridge("io", exc, stage="audio", path=path) from exc
    return written


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
# Public entry point
# ---------------------------------------------------------------------------


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
        written = _write_float(
            raw_path,
            _decode_range(result, stream, start_sample, end_sample, settings, source_layout),
            settings.rate,
            settings.layout,
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
