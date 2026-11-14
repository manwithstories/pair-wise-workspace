"""Standard-library PCM WAV decoding and cue alignment measurements."""

from __future__ import annotations

import math
import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import AssetError, ProjectError
from .timeline import AudioSpec, Fps, Timeline


def _round(value: float) -> float:
    rounded = round(float(value), 3)
    return 0.0 if rounded == 0.0 else rounded


@dataclass(frozen=True)
class PcmAudio:
    sample_rate: int
    channels: int
    sample_width: int
    samples: np.ndarray

    @property
    def frame_count(self) -> int:
        return int(self.samples.shape[0])


def _decode_pcm16(raw: bytes) -> np.ndarray:
    values = np.frombuffer(raw, dtype="<i2").astype(np.float32)
    return values / np.float32(32768.0)


def _decode_pcm24(raw: bytes) -> np.ndarray:
    octets = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
    values = octets[:, 0] | (octets[:, 1] << 8) | (octets[:, 2] << 16)
    values = np.where(values & 0x800000, values - 0x1000000, values)
    return values.astype(np.float32) / np.float32(8388608.0)


def decode_wav(path: str | Path) -> PcmAudio:
    target = str(path)
    try:
        with wave.open(target, "rb") as handle:
            channels = handle.getnchannels()
            sample_width = handle.getsampwidth()
            sample_rate = handle.getframerate()
            frame_count = handle.getnframes()
            compression = handle.getcomptype()
            raw = handle.readframes(frame_count)
    except (OSError, EOFError, wave.Error) as exc:
        raise AssetError(f"cannot decode WAV {target}: {exc}") from exc
    if compression != "NONE":
        raise AssetError(f"compressed WAV is unsupported: {target}")
    if sample_width not in (1, 2, 3, 4):
        raise AssetError(f"unsupported PCM width {sample_width * 8} bits: {target}")
    if channels not in (1, 2):
        raise AssetError(f"unsupported WAV channel count {channels}: {target}")
    if sample_rate <= 0 or frame_count <= 0:
        raise AssetError(f"WAV has no usable PCM frames: {target}")
    expected_bytes = frame_count * channels * sample_width
    if len(raw) < expected_bytes:
        raise AssetError(f"truncated WAV data: {target}")
    if sample_width == 1:
        samples = np.frombuffer(raw, dtype=np.uint8).astype(np.float32) / np.float32(128.0) - 1.0
    elif sample_width == 2:
        samples = _decode_pcm16(raw)
    elif sample_width == 3:
        samples = _decode_pcm24(raw)
    else:
        samples = np.frombuffer(raw, dtype="<i4").astype(np.float32) / np.float32(2147483648.0)
    samples = samples[: frame_count * channels].reshape(frame_count, channels)
    return PcmAudio(sample_rate, channels, sample_width, np.clip(samples, -1.0, 1.0))


def _first_crossing(
    samples: np.ndarray,
    expected_sample: int,
    threshold: float,
    search_ms: float,
    sample_rate: int,
) -> int:
    start = max(0, expected_sample - int(math.floor(search_ms * sample_rate / 1000.0)))
    stop = min(len(samples), expected_sample + int(math.ceil(search_ms * sample_rate / 1000.0)) + 1)
    if start >= stop:
        raise ProjectError("cue search window is outside the WAV")
    magnitude = np.max(np.abs(samples[start:stop]), axis=1)
    crossings = np.flatnonzero(magnitude > threshold)
    if crossings.size == 0:
        raise AssetError(f"cue has no sample above threshold near sample {expected_sample}")
    return start + int(crossings[0])


def check_audio(timeline: Timeline, audio_spec: AudioSpec | None = None) -> dict[str, object]:
    spec = audio_spec or timeline.audio
    if spec is None:
        raise ProjectError("project has no audio section")
    audio = decode_wav(spec.path)
    details: list[dict[str, object]] = []
    worst_cue: int | None = None
    max_abs_error = -1.0
    for cue_index, cue_frame in enumerate(spec.cues):
        expected_sample = timeline.fps.sample_index(cue_frame, audio.sample_rate)
        if expected_sample >= audio.frame_count:
            raise ProjectError(f"cue out of audio range: frame {cue_frame}")
        detected_sample = _first_crossing(
            audio.samples,
            expected_sample,
            spec.threshold,
            spec.search_ms,
            audio.sample_rate,
        )
        pts_ms = timeline.fps.pts_ms(cue_frame)
        detected_ms = detected_sample * 1000.0 / audio.sample_rate
        error_ms = detected_ms - pts_ms
        details.append(
            {
                "cue": cue_index,
                "frame": cue_frame,
                "pts_ms": _round(pts_ms),
                "sample": detected_sample,
                "detected_ms": _round(detected_ms),
                "error_ms": _round(error_ms),
            }
        )
        if abs(error_ms) > max_abs_error:
            max_abs_error = abs(error_ms)
            worst_cue = cue_index
    return {
        "cues": len(details),
        "max_abs_error_ms": _round(max(0.0, max_abs_error)),
        "worst_cue": worst_cue,
        "cue_details": details,
    }
