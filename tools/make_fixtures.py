#!/usr/bin/env python3
"""Generate deterministic local fixtures; no external audio is referenced."""

from __future__ import annotations

import argparse
from pathlib import Path
import sys

import numpy as np
import soundfile as sf

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from audio_pipeline.loudness import LoudnessMeter, RmsMeter  # noqa: E402
from audio_pipeline.resampler import resample_array  # noqa: E402


DURATION = 2.2
PROGRAM_END = 1.9
TARGET_LUFS = -27.1
TARGET_RMS = -23.4


def _meter(block: np.ndarray, loudness: LoudnessMeter, rms: RmsMeter) -> None:
    loudness.process(block)
    rms.process(block)


def _metrics(samples: np.ndarray, rate: int) -> tuple[float, float]:
    stream = resample_array(samples, rate, 48000, block_size=1024)
    loudness = LoudnessMeter(48000, 2)
    rms = RmsMeter(2)
    for start in range(0, len(stream), 1024):
        _meter(stream[start : start + 1024], loudness, rms)
    return loudness.finalize(), rms.finalize()


def _program(rate: int, tail: str) -> tuple[np.ndarray, np.ndarray]:
    frames = int(round(rate * DURATION))
    end = int(round(rate * PROGRAM_END))
    time = np.arange(frames, dtype=np.float64) / rate
    envelope = np.minimum(1.0, time / 0.030) * np.minimum(
        1.0, np.maximum(0.0, (PROGRAM_END - time) / 0.030)
    )
    carrier = 0.8 * np.sin(2.0 * np.pi * 60.0 * time) + 0.2 * np.sin(
        2.0 * np.pi * 120.0 * time
    )
    left = 0.5 * carrier * envelope * (0.92 + 0.08 * np.sin(2.0 * np.pi * 2.3 * time))
    right = (
        0.5
        * carrier
        * envelope
        * (0.92 + 0.08 * np.sin(2.0 * np.pi * 1.7 * time + 0.4))
    )
    active = np.zeros(frames, dtype=bool)
    active[:end] = True
    if tail == "dc":
        left[end:] = 0.012
        right[end:] = 0.012
    else:
        left[end:] = 0.0
        right[end:] = 0.0
    return np.stack((left, right), axis=1), active


def _fit(samples: np.ndarray, rate: int, active: np.ndarray) -> np.ndarray:
    loudness, rms = _metrics(samples, rate)
    scale = 10.0 ** ((TARGET_LUFS - loudness) / 20.0)
    samples = samples * scale
    loudness, rms = _metrics(samples, rate)
    current_rms = 10.0 ** (rms / 20.0)
    desired_rms = 10.0 ** (TARGET_RMS / 20.0)
    if desired_rms > current_rms:
        active_ratio = max(0.001, np.count_nonzero(active) / len(samples))
        dc = np.sqrt((desired_rms**2 - current_rms**2) / active_ratio)
        samples = samples.copy()
        samples[active] += dc
    return np.clip(samples, -0.95, 0.95)


def _write(path: Path, samples: np.ndarray, rate: int, subtype: str) -> None:
    sf.write(path, samples, rate, format="WAV", subtype=subtype)


def generate(directory: Path) -> list[Path]:
    directory.mkdir(parents=True, exist_ok=True)
    generated: list[Path] = []
    specifications = [
        ("mono_8k_u8.wav", 8000, "PCM_U8", "mono", "zero"),
        ("stereo_16k_pcm16.wav", 16000, "PCM_16", "stereo", "dc"),
        ("mono_22050_pcm24.wav", 22050, "PCM_24", "mono", "zero"),
        ("stereo_44100_pcm24.wav", 44100, "PCM_24", "stereo", "zero"),
        ("stereo_96000_pcm24.wav", 96000, "PCM_24", "stereo", "dc"),
    ]
    for name, rate, subtype, layout, tail in specifications:
        samples, active = _program(rate, tail)
        samples = _fit(samples, rate, active)
        if layout == "mono":
            samples = samples[:, :1]
        path = directory / name
        _write(path, samples, rate, subtype)
        generated.append(path)

    rate = 48000
    samples, active = _program(rate, "none")
    samples = _fit(samples, rate, active)
    samples[2000:2120, 0] = 0.999
    path = directory / "stereo_48000_pcm24_clipped.wav"
    _write(path, samples, rate, "PCM_24")
    generated.append(path)
    return generated


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", nargs="?", type=Path, default=Path("fixtures"))
    args = parser.parse_args(argv)
    for path in generate(args.directory):
        print(path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
