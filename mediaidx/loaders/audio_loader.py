"""WAV/FLAC/OGG decoding, downmix, NumPy resampling and silence trimming."""

from __future__ import annotations

import io
from dataclasses import dataclass
from pathlib import Path

import numpy as np
import soundfile as sf

from mediaidx.canonical import AUDIO_FORMATS, MediaDecodeError, MediaFamily


TARGET_SAMPLE_RATE = 16_000


@dataclass
class NormalizedAudio:
    samples: np.ndarray
    sample_rate: int
    source_sample_rate: int
    source_channels: int
    source_duration: float
    trim_start: float = 0.0
    trim_end: float = 0.0

    @property
    def duration(self) -> float:
        return float(self.samples.size) / float(self.sample_rate)

    def metadata(self) -> dict[str, object]:
        return {
            "source_sample_rate": self.source_sample_rate,
            "source_channels": self.source_channels,
            "source_duration": self.source_duration,
            "duration": self.duration,
            "sample_rate": self.sample_rate,
            "trim_start": self.trim_start,
            "trim_end": self.trim_end,
        }


def resample_audio(
    samples: np.ndarray, source_rate: int, target_rate: int = TARGET_SAMPLE_RATE
) -> np.ndarray:
    """Band-limited FFT resampling implemented only with NumPy."""

    if source_rate <= 0:
        raise MediaDecodeError("corrupt_media", f"invalid sample rate {source_rate}")
    if source_rate == target_rate:
        return np.ascontiguousarray(samples, dtype=np.float32)
    n_in = int(samples.size)
    if n_in == 0:
        return np.empty(0, dtype=np.float32)
    n_out = max(1, int(round(n_in * target_rate / source_rate)))
    spectrum = np.fft.rfft(samples.astype(np.float64))
    out_bins = n_out // 2 + 1
    new_spectrum = np.zeros(out_bins, dtype=np.complex128)
    copy_bins = min(spectrum.size, out_bins)
    new_spectrum[:copy_bins] = spectrum[:copy_bins]
    result = np.fft.irfft(new_spectrum, n=n_out) * (n_out / n_in)
    return np.ascontiguousarray(result, dtype=np.float32)


def trim_silence(samples: np.ndarray, sample_rate: int = TARGET_SAMPLE_RATE) -> tuple[np.ndarray, int, int]:
    """Trim only leading/trailing low-energy frames, retaining one frame of pad."""

    if samples.size == 0:
        return samples, 0, 0
    frame_len = max(1, int(round(sample_rate * 0.02)))
    usable = samples.size // frame_len
    if usable < 2:
        return samples, 0, 0
    framed = samples[: usable * frame_len].reshape(usable, frame_len)
    rms = np.sqrt(np.mean(np.square(framed, dtype=np.float64), axis=1))
    peak = float(np.max(rms)) if rms.size else 0.0
    if peak <= 1e-7:
        return samples, 0, 0
    active = rms > max(1e-6, peak * 0.01)
    active_indices = np.flatnonzero(active)
    if active_indices.size == 0:
        return samples, 0, 0
    first = max(0, int(active_indices[0]) - 1)
    last = min(usable, int(active_indices[-1]) + 2)
    start = first * frame_len
    end = min(samples.size, last * frame_len)
    return np.ascontiguousarray(samples[start:end]), start, samples.size - end


def decode_audio_bytes(data: bytes, *, source_path: Path | None = None) -> NormalizedAudio:
    del source_path
    try:
        with sf.SoundFile(io.BytesIO(data)) as sound:
            source_rate = int(sound.samplerate)
            channels = int(sound.channels)
            source_frames = int(sound.frames)
            pcm = sound.read(dtype="float32", always_2d=True)
    except Exception as exc:
        error_name = type(exc).__name__
        if "Libsndfile" in error_name or "SoundFile" in error_name:
            raise MediaDecodeError("corrupt_media", f"audio decode failed: {exc}") from exc
        raise MediaDecodeError("corrupt_media", f"audio container could not be opened: {exc}") from exc

    if pcm.size == 0 or channels <= 0:
        raise MediaDecodeError("corrupt_media", "audio contains no frames")
    pcm = np.nan_to_num(pcm, nan=0.0, posinf=0.0, neginf=0.0)
    mono = pcm.mean(axis=1, dtype=np.float64).astype(np.float32)
    if not np.isfinite(mono).all():
        raise MediaDecodeError("corrupt_media", "audio samples are not finite")
    source_duration = source_frames / source_rate if source_rate else 0.0
    converted = resample_audio(mono, source_rate, TARGET_SAMPLE_RATE)
    trimmed, trim_start, trim_end = trim_silence(converted, TARGET_SAMPLE_RATE)
    return NormalizedAudio(
        samples=trimmed,
        sample_rate=TARGET_SAMPLE_RATE,
        source_sample_rate=source_rate,
        source_channels=channels,
        source_duration=source_duration,
        trim_start=trim_start / TARGET_SAMPLE_RATE,
        trim_end=trim_end / TARGET_SAMPLE_RATE,
    )


def decode_audio_file(path: Path, data: bytes | None = None) -> NormalizedAudio:
    if data is None:
        try:
            data = path.read_bytes()
        except OSError as exc:
            raise MediaDecodeError("io_error", str(exc), retryable=True) from exc
    return decode_audio_bytes(data, source_path=path)


class AudioLoader:
    name = "audio-libsndfile"
    formats = tuple(sorted(AUDIO_FORMATS))

    def register_into(self, registry: object) -> None:
        registry.register(  # type: ignore[attr-defined]
            MediaFamily.AUDIO,
            self.formats,
            self.name,
            decode_audio_file,
            metadata={"loader": self.name},
        )


AUDIO_LOADER = AudioLoader()
load_audio = decode_audio_file
