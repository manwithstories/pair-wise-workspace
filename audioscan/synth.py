"""Deterministic synthetic WAV fixtures with known speech/silence truth."""

from __future__ import annotations

import os
import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np


PROFILES = (
    (22050, 1, "pcm16"),
    (44100, 2, "pcm24"),
    (48000, 2, "float32"),
    (96000, 2, "pcm32"),
    (44100, 1, "pcm24"),
    (48000, 1, "pcm32"),
)
SEGMENT_PATTERN = (4, 6, 5, 5, 4, 5, 6, 5)


@dataclass(frozen=True)
class SyntheticFixture:
    path: str
    sample_rate: int
    channels: int
    encoding: str
    total_samples: int
    segments: tuple[tuple[int, int], ...]
    silence_samples: int

    @property
    def segment_count(self) -> int:
        return len(self.segments)


def _pack_wav(samples: np.ndarray, sample_rate: int, encoding: str) -> bytes:
    channels = samples.shape[1]
    if encoding == "pcm16":
        tag, bits = 1, 16
        payload = np.clip(np.round(samples * 32768.0), -32768, 32767).astype("<i2").tobytes()
    elif encoding == "pcm24":
        tag, bits = 1, 24
        ints = np.clip(np.round(samples * 8388608.0), -8388608, 8388607).astype(np.int32)
        unsigned = np.where(ints < 0, ints + 0x1000000, ints).astype(np.uint32).ravel()
        raw = np.empty((ints.size, 3), dtype=np.uint8)
        raw[:, 0] = unsigned & 0xFF
        raw[:, 1] = (unsigned >> 8) & 0xFF
        raw[:, 2] = (unsigned >> 16) & 0xFF
        payload = raw.tobytes()
    elif encoding == "pcm32":
        tag, bits = 1, 32
        payload = np.clip(np.round(samples * 2147483648.0), -2147483648, 2147483647).astype("<i4").tobytes()
    elif encoding == "float32":
        tag, bits = 3, 32
        payload = samples.astype("<f4").tobytes()
    else:
        raise ValueError(f"unsupported synthetic encoding: {encoding}")
    block_align = channels * bits // 8
    fmt = struct.pack("<HHIIHH", tag, channels, sample_rate, sample_rate * block_align, block_align, bits)
    junk = b"JUNK" + b"xyz"
    chunks = b"fmt " + struct.pack("<I", len(fmt)) + fmt
    chunks += b"JUNK" + struct.pack("<I", len(junk)) + junk + b"\x00"
    chunks += b"data" + struct.pack("<I", len(payload)) + payload
    if len(payload) & 1:
        chunks += b"\x00"
    return b"RIFF" + struct.pack("<I", 4 + len(chunks)) + b"WAVE" + chunks


def _voice_signal(rng: np.random.Generator, sample_rate: int, seconds: float, level_db: float) -> np.ndarray:
    count = max(1, int(round(seconds * sample_rate)))
    t = np.arange(count, dtype=np.float64) / sample_rate
    f0 = rng.uniform(105.0, 220.0)
    signal = np.zeros(count, dtype=np.float64)
    for harmonic in range(1, 8):
        gain = rng.uniform(0.15, 1.0) / harmonic
        signal += gain * np.sin(2.0 * np.pi * f0 * harmonic * t + rng.uniform(0, 2 * np.pi))
    syllable = 0.65 + 0.35 * np.sin(2.0 * np.pi * rng.uniform(3.0, 6.0) * t + rng.uniform(0, 2 * np.pi))
    signal *= syllable
    signal += 0.04 * rng.standard_normal(count)
    rms = max(float(np.sqrt(np.mean(signal * signal))), 1e-9)
    return (signal * (10.0 ** (level_db / 20.0) / rms)).astype(np.float32)


def _apply_fades(signal: np.ndarray, sample_rate: int, fade_in: float, fade_out: float) -> None:
    in_count = min(signal.size, max(0, int(round(fade_in * sample_rate))))
    out_count = min(signal.size, max(0, int(round(fade_out * sample_rate))))
    if in_count:
        signal[:in_count] *= np.linspace(0.0, 1.0, in_count, dtype=np.float32)
    if out_count:
        signal[-out_count:] *= np.linspace(1.0, 0.0, out_count, dtype=np.float32)


def generate_fixture(path: str | os.PathLike[str], seed: int, index: int, duration_scale: float = 1.0) -> SyntheticFixture:
    """Generate one deterministic fixture and return its known intervals."""

    rng = np.random.default_rng(int(seed) * 100_003 + int(index))
    sample_rate, channels, encoding = PROFILES[int(index) % len(PROFILES)]
    segment_count = SEGMENT_PATTERN[(int(index) + int(seed)) % len(SEGMENT_PATTERN)]
    frame_samples = max(1, int(round(sample_rate * 0.01)))
    lead = float(rng.uniform(0.35, 1.0))
    tail = float(rng.uniform(0.5, 1.2))
    gaps = [float(rng.uniform(0.4, 2.8)) for _ in range(segment_count - 1)]
    voices = [float(rng.uniform(1.8, 4.2)) for _ in range(segment_count)]
    scale = max(0.05, float(duration_scale))
    lead, tail = lead * scale, tail * scale
    gaps = [gap * scale for gap in gaps]
    voices = [voice * scale for voice in voices]
    seconds = lead + tail + sum(gaps) + sum(voices)
    total = max(frame_samples, int(round(seconds * sample_rate)))
    total = int(np.ceil(total / frame_samples)) * frame_samples
    flat = np.zeros((total, channels), dtype=np.float32)
    base_db = np.linspace(rng.uniform(-60.0, -56.0), rng.uniform(-50.0, -48.0), total)
    drift = 2.2 * np.sin(np.linspace(0, rng.uniform(2.0, 5.0) * np.pi, total) + rng.uniform(0, np.pi))
    noise_db = np.clip(base_db + drift, -62.0, -48.0)
    noise = rng.standard_normal((total, channels), dtype=np.float32)
    noise *= (10.0 ** (noise_db / 20.0)).astype(np.float32)[:, None]
    flat[:] = noise
    if channels == 2 and index % 5 == 4:
        flat[:, 1] *= 0.0005

    cursor = int(round(lead * sample_rate))
    truth: list[tuple[int, int]] = []
    for segment_index in range(segment_count):
        if segment_index:
            cursor += int(round(gaps[segment_index - 1] * sample_rate))
        start = int(np.ceil(cursor / frame_samples)) * frame_samples
        voice_frames = max(1, int(round(voices[segment_index] * sample_rate / frame_samples)))
        voice = _voice_signal(rng, sample_rate, voice_frames * frame_samples / sample_rate, float(rng.uniform(-14.0, -8.0)))
        fade_in = float(rng.uniform(0.15, 0.25))
        fade_out = float(rng.uniform(0.15, 0.25))
        _apply_fades(voice, sample_rate, fade_in, fade_out)
        if channels == 2:
            delay = max(1, int(round(rng.uniform(0.0, 0.002) * sample_rate)))
            voice2 = np.roll(voice, delay)
            if delay:
                voice2[:delay] = 0.0
            pair = np.stack((voice, voice2 * np.float32(rng.uniform(0.86, 1.0))), axis=1)
        else:
            pair = voice[:, None]
        end = min(total, start + pair.shape[0])
        flat[start:end] += pair[: end - start]
        truth.append((start, end))
        cursor = end

    silence = total - sum(end - start for start, end in truth)
    np.clip(flat, -1.0, 1.0, out=flat)
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(_pack_wav(flat, sample_rate, encoding))
    return SyntheticFixture(str(target), sample_rate, channels, encoding, total, tuple(truth), silence)


def generate_many(out_dir: str | os.PathLike[str], files: int, seed: int, duration_scale: float = 1.0) -> list[SyntheticFixture]:
    directory = Path(out_dir)
    directory.mkdir(parents=True, exist_ok=True)
    generated = []
    for index in range(int(files)):
        path = directory / f"fx_{index:02d}.wav"
        generated.append(generate_fixture(path, seed, index, duration_scale))
    return generated
