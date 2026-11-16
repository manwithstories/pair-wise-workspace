"""Deterministic WAV fixtures generated on demand.

Every sample is synthesised in code -- no external assets -- so demos and
tests are reproducible.  The set exercises the cases the detector must get
right: low-level room tone, slow fades, a stereo 24-bit file at 44.1 kHz and a
very short noise pulse between two real bursts.
"""

from __future__ import annotations

import os
import struct
import wave
from dataclasses import dataclass
from typing import Callable, Optional

import numpy as np

FIXTURE_NAMES = (
    "s1_dialogue.wav",
    "s2_ambience.wav",
    "s3_sfx.wav",
)


@dataclass(frozen=True)
class FixtureSpec:
    name: str
    sample_rate: int
    channels: int
    sample_width: int
    build: Callable[[int, np.random.Generator], np.ndarray]


def _tone(sr: int, seconds: float, freq: float, amp: float) -> np.ndarray:
    n = int(round(sr * seconds))
    t = np.arange(n, dtype=np.float64) / sr
    return (amp * np.sin(2 * np.pi * freq * t)).astype(np.float32)


def _noise(sr: int, seconds: float, amp: float, rng: np.random.Generator) -> np.ndarray:
    n = int(round(sr * seconds))
    return (rng.standard_normal(n) * amp).astype(np.float32)


def _raised_cosine(n: int) -> np.ndarray:
    if n <= 1:
        return np.ones(max(n, 0), dtype=np.float32)
    ramp = 0.5 - 0.5 * np.cos(np.pi * np.arange(n) / (n - 1))
    return ramp.astype(np.float32)


def _with_fades(
    signal: np.ndarray, sr: int, fade_in_s: float, fade_out_s: float
) -> np.ndarray:
    out = signal.astype(np.float32, copy=True)
    n_in = min(out.size, int(round(sr * fade_in_s)))
    n_out = min(out.size, int(round(sr * fade_out_s)))
    if n_in:
        out[:n_in] *= _raised_cosine(n_in)
    if n_out:
        out[-n_out:] *= _raised_cosine(n_out)[::-1]
    return out


def _silence(sr: int, seconds: float) -> np.ndarray:
    return np.zeros(int(round(sr * seconds)), dtype=np.float32)


def build_dialogue(sr: int, rng: np.random.Generator) -> np.ndarray:
    """Mono 48 kHz: two faded utterances over a quiet room tone."""

    room = _noise(sr, 0.35, 0.0012, rng)
    line_a = _with_fades(_tone(sr, 0.65, 180, 0.32), sr, 0.12, 0.10)
    gap = _noise(sr, 0.55, 0.0010, rng)
    line_b = _with_fades(_tone(sr, 0.55, 320, 0.28), sr, 0.08, 0.20)
    tail = _noise(sr, 0.40, 0.0011, rng)
    return np.concatenate([room, line_a, gap, line_b, tail])


def build_ambience(sr: int, rng: np.random.Generator) -> np.ndarray:
    """Stereo 44.1 kHz/24-bit: a soft swell, 0.8 s of quiet, a second swell."""

    room = _noise(sr, 0.30, 0.0015, rng)
    swell = _with_fades(_noise(sr, 0.75, 0.16, rng), sr, 0.25, 0.25)
    gap = _noise(sr, 0.80, 0.0014, rng)
    swell2 = _with_fades(_noise(sr, 0.60, 0.13, rng), sr, 0.20, 0.20)
    tail = _noise(sr, 0.25, 0.0015, rng)
    mono = np.concatenate([room, swell, gap, swell2, tail])
    # Slight decorrelation between channels keeps the downmix realistic.
    right = np.roll(mono, 7) * 0.92
    return np.stack([mono, right], axis=1)


def build_sfx(sr: int, rng: np.random.Generator) -> np.ndarray:
    """Mono 48 kHz: two short effects plus a 25 ms noise pulse between them.

    Noise amplitudes stay below 0.2 so a ~3-sigma tail never reaches full
    scale; the fixture must measure the detector, not PCM clipping.
    """

    room = _noise(sr, 0.30, 0.0010, rng)
    hit = _with_fades(_noise(sr, 0.18, 0.22, rng), sr, 0.004, 0.06)
    brief = _silence(sr, 0.30)
    pulse = _with_fades(_noise(sr, 0.025, 0.18, rng), sr, 0.002, 0.002)
    brief2 = _silence(sr, 0.35)
    hit2 = _with_fades(_noise(sr, 0.14, 0.20, rng), sr, 0.004, 0.05)
    tail = _noise(sr, 0.30, 0.0010, rng)
    return np.concatenate([room, hit, brief, pulse, brief2, hit2, tail])


FIXTURES: tuple[FixtureSpec, ...] = (
    FixtureSpec("s1_dialogue.wav", 48000, 1, 2, build_dialogue),
    FixtureSpec("s2_ambience.wav", 44100, 2, 3, build_ambience),
    FixtureSpec("s3_sfx.wav", 48000, 1, 2, build_sfx),
)


def _encode(samples: np.ndarray, sample_width: int) -> bytes:
    clipped = np.clip(samples, -1.0, 1.0)
    if sample_width == 2:
        ints = np.round(clipped * 32767.0).astype("<i2")
        return ints.tobytes()
    if sample_width == 3:
        ints = np.round(clipped * 8388607.0).astype(np.int32).reshape(-1)
        raw = ints.view(np.uint8).reshape(-1, 4)[:, :3]
        return raw.tobytes()
    raise ValueError(f"unsupported sample width {sample_width}")


def write_wav(
    path: str, samples: np.ndarray, sample_rate: int, sample_width: int
) -> str:
    """Write ``(frames, channels)`` float data as integer PCM."""

    if samples.ndim == 1:
        samples = samples.reshape(-1, 1)
    channels = int(samples.shape[1])
    payload = _encode(samples, sample_width)
    with wave.open(path, "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(sample_width)
        handle.setframerate(sample_rate)
        handle.writeframes(payload)
    return path


def generate_fixtures(
    out_dir: str, seed: int = 20240617, names: Optional[tuple[str, ...]] = None
) -> list[str]:
    """Create the fixture set under ``out_dir``; returns the written paths."""

    os.makedirs(out_dir, exist_ok=True)
    wanted = set(names) if names else None
    written: list[str] = []
    for offset, spec in enumerate(FIXTURES):
        if wanted is not None and spec.name not in wanted:
            continue
        rng = np.random.default_rng(seed + offset)
        samples = spec.build(spec.sample_rate, rng)
        path = os.path.join(out_dir, spec.name)
        write_wav(path, samples, spec.sample_rate, spec.sample_width)
        written.append(path)
    return written


def write_float_wav(path: str, frames: int = 64, sr: int = 8000) -> str:
    """Deliberately unsupported file, used by negative tests."""

    payload = struct.pack("<" + "f" * frames, *([0.0] * frames))
    header = (
        b"RIFF"
        + struct.pack("<I", 36 + len(payload))
        + b"WAVEfmt "
        + struct.pack("<IHHIIHH", 16, 3, 1, sr, sr * 4, 4, 32)
        + b"data"
        + struct.pack("<I", len(payload))
    )
    with open(path, "wb") as fh:
        fh.write(header + payload)
    return path


def write_truncated_wav(path: str, sr: int = 8000) -> str:
    """Header claims 100 frames, payload holds 2."""

    header = (
        b"RIFF"
        + struct.pack("<I", 36 + 4)
        + b"WAVEfmt "
        + struct.pack("<IHHIIHH", 16, 1, 1, sr, sr * 2, 2, 16)
        + b"data"
        + struct.pack("<I", 200)
    )
    with open(path, "wb") as fh:
        fh.write(header + b"\x00" * 4)
    return path
