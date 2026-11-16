"""Shared pytest fixtures: a small deterministic project built in tmp_path."""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf


def _bed(rate: int, channels: int, seconds: float, seed: int, peak: float) -> np.ndarray:
    rng = np.random.default_rng(seed)
    frames = int(rate * seconds)
    t = np.arange(frames, dtype=np.float64) / rate
    out = np.zeros((frames, channels), dtype=np.float64)
    for c in range(channels):
        out[:, c] = peak * (
            0.7 * np.sin(2 * np.pi * (180 + 40 * c) * t + c)
            + 0.3 * np.sin(2 * np.pi * (360 + 40 * c) * t)
        )
    out += rng.standard_normal(out.shape) * peak * 0.05
    return out


@pytest.fixture()
def fixture_dir(tmp_path: Path) -> Path:
    """Four sources covering 44.1/48/96 kHz and mono/stereo/quad."""
    out = tmp_path / "fixtures"
    out.mkdir()
    sources = {
        "a.wav": (44100, 1, 1.2, 1, 0.9),
        "b.wav": (48000, 2, 1.1, 2, 0.8),
        "c.wav": (96000, 4, 1.0, 3, 0.7),
        "d.wav": (48000, 2, 0.9, 4, 0.6),
    }
    for name, (rate, channels, seconds, seed, peak) in sources.items():
        signal = _bed(rate, channels, seconds, seed, peak)
        signal[0] = 0.0
        signal[-1] = 0.0
        sf.write(str(out / name), signal, rate, subtype="FLOAT", format="WAV")
    project = {
        "schema": "mix-project/1",
        "target_rate": 48000,
        "tracks": [
            {
                "name": "drums",
                "file": "a.wav",
                "layout": "mono",
                "gain_db": 0.0,
                "automation": [
                    {"time": 0.0, "gain_db": -2.0},
                    {"time": 0.45, "gain_db": 3.0},
                    {"time": 0.9, "gain_db": -1.0},
                ],
                "max_slew_db_per_s": 40.0,
            },
            {
                "name": "bass",
                "file": "b.wav",
                "layout": "stereo",
                "start_time": 0.05,
                "gain_db": -1.0,
                "automation": [
                    {"time": 0.0, "gain_db": -1.0},
                    {"time": 0.6, "gain_db": 2.0},
                ],
                "max_slew_db_per_s": 50.0,
            },
            {
                "name": "pad",
                "file": "c.wav",
                "layout": "quad",
                "start_time": 0.1,
                "gain_db": -2.0,
                "silences": [[0.5, 0.62]],
                "automation": [
                    {"time": 0.0, "gain_db": -2.0},
                    {"time": 0.7, "gain_db": 0.5},
                ],
                "max_slew_db_per_s": 60.0,
            },
            {
                "name": "fx",
                "file": "d.wav",
                "layout": "stereo",
                "start_time": 0.0,
                "gain_db": -3.0,
                "clips": [
                    {"path": "d.wav", "start_time": 0.0, "source_offset_frames": 0,
                     "length_frames": 12000},
                    {"path": "d.wav", "start_time": 0.4, "source_offset_frames": 20000,
                     "length_frames": 12000},
                ],
                "automation": [
                    {"time": 0.0, "gain_db": -3.0},
                    {"time": 0.8, "gain_db": -0.5},
                ],
                "max_slew_db_per_s": 60.0,
            },
        ],
    }
    (out / "project.json").write_text(json.dumps(project), encoding="utf-8")
    return out


@pytest.fixture()
def project(fixture_dir: Path):
    from src.mixer.model import parse_project

    raw = json.loads((fixture_dir / "project.json").read_text())
    return parse_project(raw, base_dir=fixture_dir)
