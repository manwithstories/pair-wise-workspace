"""Deterministic sample fixtures for the mix-master exercise.

Generates four WAV sources at different rates/layouts plus ``project.json``.
Everything is synthesised in-process from a seed: no external assets, no
network. Bursts are placed so the limiter has real work, blank spans exercise
silence masking, and one track is split into clips to exercise offsets.
"""

from __future__ import annotations

import argparse
import json
import math
import sys
from pathlib import Path
from typing import Any, Sequence

import numpy as np
import soundfile as sf

DEFAULT_SEED = 17
TRACK_SPECS = (
    # rate, channels, layout, seconds, peak, role
    (44100, 1, "mono", 4.0, 0.95, "kick/body pulse"),
    (48000, 2, "stereo", 3.6, 0.85, "bass bed"),
    (96000, 4, "quad", 3.2, 0.80, "wide vocal-ish pad"),
    (48000, 2, "stereo", 3.0, 0.70, "fx stabs (two clips)"),
)


def _tone_bed(rng: np.random.Generator, frames: int, rate: int, channels: int, base_hz: float) -> np.ndarray:
    """A slowly moving harmonic bed plus filtered noise, per channel."""
    t = np.arange(frames, dtype=np.float64) / rate
    out = np.zeros((frames, channels), dtype=np.float64)
    noise = rng.standard_normal((frames, channels))
    # One-pole smoothing keeps the noise band-limited so linear resampling
    # stays well behaved and the result is reproducible.
    smoothed = np.zeros_like(noise)
    state = np.zeros(channels)
    alpha = 0.05
    for i in range(frames):
        state = state + alpha * (noise[i] - state)
        smoothed[i] = state
    for c in range(channels):
        detune = 1.0 + 0.01 * c
        tone = (
            0.7 * np.sin(2.0 * math.pi * base_hz * detune * t)
            + 0.2 * np.sin(2.0 * math.pi * base_hz * 2.0 * detune * t + 0.5 * c)
        )
        tremolo = 0.75 + 0.25 * np.sin(2.0 * math.pi * (0.6 + 0.05 * c) * t + c)
        out[:, c] = tone * tremolo * 0.8 + smoothed[:, c] * 0.8
    return out


def _apply_envelope(signal: np.ndarray, rate: int, seed: int) -> np.ndarray:
    """Short fades at both ends so clips don't start/stop with a hard edge."""
    fade = min(int(0.01 * rate), signal.shape[0] // 2)
    if fade <= 0:
        return signal
    ramp = np.linspace(0.0, 1.0, fade)[:, None]
    signal[:fade] *= ramp
    signal[-fade:] *= ramp[::-1]
    return signal


def _insert_bursts(
    signal: np.ndarray, rng: np.random.Generator, rate: int, count: int, scale: float
) -> tuple[np.ndarray, list[dict[str, Any]]]:
    """Add short transient peaks and report where they landed."""
    frames = signal.shape[0]
    bursts: list[dict[str, Any]] = []
    for i in range(count):
        length = int(rate * (0.012 + 0.01 * rng.random()))
        start = int(rng.integers(0, max(1, frames - length - 1)))
        burst = rng.standard_normal((length, signal.shape[1]))
        window = np.hanning(length)[:, None]
        amplitude = scale * (1.2 + 0.8 * rng.random())
        signal[start:start + length] += burst * window * amplitude
        bursts.append(
            {
                "start_frame": start,
                "length_frames": length,
                "amplitude": round(float(amplitude), 4),
            }
        )
    return signal, bursts


def _blank_span(signal: np.ndarray, rate: int, start_s: float, end_s: float) -> np.ndarray:
    """Zero a span to create a deliberate blank segment."""
    out = signal.copy()
    lo = int(start_s * rate)
    hi = min(out.shape[0], int(end_s * rate))
    if hi > lo:
        out[lo:hi] = 0.0
    return out


def _write_wav(path: Path, signal: np.ndarray, rate: int) -> None:
    sf.write(str(path), signal, rate, subtype="FLOAT", format="WAV")


def build_track(
    out_dir: Path, spec: Sequence[Any], rng: np.random.Generator, index: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    rate, channels, layout, seconds, peak, role = spec
    frames = int(round(seconds * rate))
    bed = _tone_bed(rng, frames, rate, channels, base_hz=110.0 + 55.0 * index)
    bed = _apply_envelope(bed, rate, index)
    bed = _insert_bursts(bed, rng, rate, count=2 + index, scale=peak * 2.5)[0]
    if index == 2:  # give the quad pad a blank stretch to exercise silence
        bed = _blank_span(bed, rate, 1.2, 1.6)
    bed *= peak / max(peak, float(np.abs(bed).max())) if np.abs(bed).max() > peak else 1.0

    file_name = f"track{index + 1}_{layout}_{rate}.wav"
    _write_wav(out_dir / file_name, bed, rate)
    meta = {
        "name": ["drums", "bass", "pad", "fx"][index],
        "file": file_name,
        "role": role,
        "rate": rate,
        "channels": channels,
        "layout": layout,
        "frames": frames,
        "duration_s": round(frames / rate, 4),
        "peak": round(float(np.abs(bed).max()), 4),
    }
    return meta, {"signal": bed, "rate": rate}


def build_project(out_dir: Path, seed: int = DEFAULT_SEED) -> list[dict[str, Any]]:
    out_dir.mkdir(parents=True, exist_ok=True)
    specs = []
    for index, spec in enumerate(TRACK_SPECS):
        rng = np.random.default_rng(seed + 101 * index)
        specs.append(build_track(out_dir, spec, rng, index))

    tracks = [
        {
            "name": specs[0][0]["name"],
            "file": specs[0][0]["file"],
            "layout": "mono",
            "start_time": 0.0,
            "gain_db": 1.0,
            "automation": [
                {"time": 0.0, "gain_db": 1.0},
                {"time": 0.9, "gain_db": 4.5},
                {"time": 1.7, "gain_db": 2.5},
                {"time": 2.6, "gain_db": 4.0},
                {"time": 3.4, "gain_db": 2.0},
            ],
            "max_slew_db_per_s": 40.0,
        },
        {
            "name": specs[1][0]["name"],
            "file": specs[1][0]["file"],
            "layout": "stereo",
            "start_time": 0.10,
            "gain_db": 2.0,
            "automation": [
                {"time": 0.0, "gain_db": 0.0},
                {"time": 0.8, "gain_db": 3.5},
                {"time": 1.6, "gain_db": 1.5},
                {"time": 2.4, "gain_db": 3.0},
                {"time": 2.4, "gain_db": 2.5},  # duplicate timestamp: last wins
                {"time": 3.2, "gain_db": 1.0},
            ],
            "max_slew_db_per_s": 60.0,
        },
        {
            "name": specs[2][0]["name"],
            "file": specs[2][0]["file"],
            "layout": "quad",
            "start_time": 0.25,
            "gain_db": -1.0,
            "silences": [[1.1, 1.5]],
            "channel_matrix": [[1.0, 0.0], [0.0, 1.0], [0.35, 0.0], [0.0, 0.35]],
            "automation": [
                {"time": 0.0, "gain_db": -2.0},
                {"time": 1.0, "gain_db": 1.0},
                {"time": 1.9, "gain_db": -0.5},
                {"time": 2.8, "gain_db": 3.0},
            ],
            "max_slew_db_per_s": 50.0,
        },
        {
            "name": specs[3][0]["name"],
            "file": specs[3][0]["file"],
            "layout": "stereo",
            "start_time": 0.6,
            "gain_db": -2.0,
            "clips": [
                {
                    "path": specs[3][0]["file"],
                    "start_time": 0.0,
                    "source_offset_frames": 0,
                    "length_frames": 24000,
                },
                {
                    "path": specs[3][0]["file"],
                    "start_time": 0.5,
                    "source_offset_frames": 48000,
                    "length_frames": 24000,
                },
            ],
            "automation": [
                {"time": 0.0, "gain_db": -4.0},
                {"time": 1.2, "gain_db": 0.0},
                {"time": 2.2, "gain_db": 2.0},
            ],
            "max_slew_db_per_s": 80.0,
        },
    ]
    project = {
        "schema": "mix-project/1",
        "target_rate": 48000,
        "seed": seed,
        "tracks": tracks,
    }
    (out_dir / "project.json").write_text(json.dumps(project, indent=2) + "\n", encoding="utf-8")

    manifest = {
        "seed": seed,
        "target_rate": 48000,
        "tracks": [meta for meta, _ in specs],
        "project": str(out_dir / "project.json"),
    }
    (out_dir / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return tracks


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="fixtures.generate_samples", description="Generate mixer sample fixtures"
    )
    parser.add_argument("--out", required=True, help="output directory for fixtures")
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED, help="fixture seed")
    args = parser.parse_args(argv)
    tracks = build_project(Path(args.out), seed=args.seed)
    for track in tracks:
        print(f"track={track['name']} file={track['file']} start={track['start_time']}")
    print(f"wrote {len(tracks)} tracks + project.json to {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
