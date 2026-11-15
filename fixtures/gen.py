"""Generate deterministic, code-only audio fixtures and a project manifest."""

from __future__ import annotations

import json
import math
from pathlib import Path

import numpy as np
import soundfile as sf


def _fade(signal: np.ndarray, sample_rate: int, seconds: float = 0.02) -> np.ndarray:
    width = min(signal.shape[0] // 2, max(1, int(sample_rate * seconds)))
    if width == 0:
        return signal
    ramp = np.linspace(0.0, 1.0, width, endpoint=False)[:, None]
    output = signal.copy()
    output[:width] *= ramp
    output[-width:] *= ramp[::-1]
    return output


def _normalise(signal: np.ndarray, peak: float, sample_rate: int) -> np.ndarray:
    maximum = float(np.max(np.abs(signal)))
    if maximum > 0.0:
        signal = signal * (peak / maximum)
    return _fade(signal, sample_rate)


def _dialogue(sample_rate: int, seconds: float, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    time = np.arange(int(round(sample_rate * seconds)), dtype=np.float64) / sample_rate
    syllable = 0.35 + 0.65 * np.sin(2.0 * np.pi * 3.1 * time) ** 2
    phrase = 0.55 + 0.45 * np.sin(2.0 * np.pi * 0.37 * time + 0.4) ** 2
    voice_l = (
        0.55 * np.sin(2.0 * np.pi * 138.0 * time)
        + 0.24 * np.sin(2.0 * np.pi * 276.0 * time + 0.3)
        + 0.12 * np.sin(2.0 * np.pi * 690.0 * time + 1.1)
    )
    voice_r = (
        0.55 * np.sin(2.0 * np.pi * 141.0 * time + 0.08)
        + 0.22 * np.sin(2.0 * np.pi * 282.0 * time + 0.4)
        + 0.10 * np.sin(2.0 * np.pi * 705.0 * time + 1.0)
    )
    breath = rng.normal(0.0, 1.0, time.size)
    breath = np.convolve(breath, np.ones(24) / 24.0, mode="same")
    envelope = syllable * phrase
    return np.column_stack(
        ((voice_l + 0.04 * breath) * envelope, (voice_r + 0.04 * breath) * envelope)
    )


def _music(sample_rate: int, seconds: float, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    time = np.arange(int(round(sample_rate * seconds)), dtype=np.float64) / sample_rate
    notes = [220.0, 277.18, 329.63, 440.0, 329.63, 277.18]
    left = np.zeros_like(time)
    right = np.zeros_like(time)
    for index, frequency in enumerate(notes):
        phase = index * 0.35
        pulse = 0.5 + 0.5 * np.sin(2.0 * np.pi * 0.37 * time + phase) ** 2
        left += pulse * (
            0.34 * np.sin(2.0 * np.pi * frequency * time + phase)
            + 0.12 * np.sin(2.0 * np.pi * frequency * 2.0 * time + phase)
        )
        right += pulse * (
            0.34 * np.sin(2.0 * np.pi * frequency * 1.003 * time + phase + 0.2)
            + 0.12 * np.sin(2.0 * np.pi * frequency * 1.997 * time + phase)
        )
    shimmer = np.convolve(rng.normal(0.0, 1.0, time.size), np.ones(40) / 40.0, mode="same")
    drift = 0.92 + 0.08 * np.sin(2.0 * np.pi * 0.13 * time)
    return np.column_stack(((left + 0.015 * shimmer) * drift, (right + 0.015 * shimmer) * drift))


def _ambience(sample_rate: int, seconds: float, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    time = np.arange(int(round(sample_rate * seconds)), dtype=np.float64) / sample_rate
    noise_l = rng.normal(0.0, 1.0, time.size)
    noise_r = rng.normal(0.0, 1.0, time.size)
    window = np.hanning(81)
    window /= np.sum(window)
    noise_l = np.convolve(noise_l, window, mode="same")
    noise_r = np.convolve(noise_r, window, mode="same")
    wind_l = 0.75 * noise_l + 0.18 * np.sin(2.0 * np.pi * 82.0 * time)
    wind_r = 0.75 * noise_r + 0.16 * np.sin(2.0 * np.pi * 85.0 * time + 0.7)
    swell = 0.55 + 0.45 * np.sin(2.0 * np.pi * 0.19 * time + 1.2) ** 2
    return np.column_stack((wind_l * swell, wind_r * swell))


def _write_audio(path: Path, data: np.ndarray, sample_rate: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(path), data, sample_rate, subtype="PCM_24")


def _curve(points: list[list[float]]) -> dict:
    return {"interpolation": "linear", "points": points}


def _manifest(seed: int) -> dict:
    nodes = []
    edges = []
    tracks = []
    assets = [
        ("dialogue", "audio/dialogue_44100.wav", 44100, 2.8, 0.70,
         [[0.0, -6.0], [0.2913, -9.5], [0.8173, -4.5], [1.5037, -11.0], [2.4, -7.0]]),
        ("music", "audio/music_96000.wav", 96000, 3.2, 0.60,
         [[0.0, -11.5], [0.4711, -15.0], [1.2317, -10.5], [2.4, -13.0]]),
        ("ambience", "audio/ambience_22050.wav", 22050, 1.3, 0.50,
         [[0.0, -11.0], [0.6103, -15.0], [1.7071, -12.0], [2.4, -14.5]]),
    ]
    for track_id, path, rate, seconds, peak, points in assets:
        relevant = {"_peak": peak}
        if track_id == "dialogue":
            relevant["_seconds"] = seconds
        source_id = f"{track_id}_src"
        resample_id = f"{track_id}_resample"
        gain_id = f"{track_id}_gain"
        bus_id = f"{track_id}_bus"
        nodes.extend(
            [
                {"id": source_id, "type": "source", "path": path,
                 "sample_rate": rate, "channels": 2,
                 "event": {"start": 0.0, "end": 2.4}},
                {"id": resample_id, "type": "resample", "rate": 48000},
                {"id": gain_id, "type": "gain", "gain_db": 0.0,
                 "automation": _curve(points)},
                {"id": bus_id, "type": "bus", "gain_db": 0.0},
            ]
        )
        edges.extend(
            [
                {"from": source_id, "to": resample_id},
                {"from": resample_id, "to": gain_id},
                {"from": gain_id, "to": bus_id},
                {"from": bus_id, "to": "master_sum"},
            ]
        )
        tracks.append({"id": track_id, "source": source_id, "gain": gain_id,
                       "bus": bus_id, "mute": False, "solo": False, "_meta": relevant})
    nodes.extend(
        [
            {"id": "master_sum", "type": "sum"},
            {"id": "master_bus", "type": "bus", "gain_db": -1.0,
             "automation": _curve([[0.0, 0.0], [0.6207, -5.5], [1.1341, 0.0],
                                    [1.8803, -4.0], [2.4, -1.5]])},
            {"id": "output", "type": "output", "channels": 2},
        ]
    )
    edges.extend(
        [
            {"from": "master_sum", "to": "master_bus"},
            {"from": "master_bus", "to": "output"},
        ]
    )
    for track in tracks:
        track.pop("_meta", None)
    return {
        "name": "interactive_story_demo",
        "seed": seed,
        "sample_rate": 48000,
        "channels": 2,
        "duration": 2.4,
        "block_size": 2048,
        "clip_threshold_db": 0.0,
        "max_clip_samples": 0,
        "nodes": nodes,
        "edges": edges,
        "tracks": tracks,
        "master_bus": "master_bus",
        "output": "output",
    }


def generate(out_dir: str | Path, seed: int = 7) -> tuple[Path, list[Path]]:
    """Write all fixtures and return the manifest path plus asset paths."""
    root = Path(out_dir)
    audio = root / "audio"
    audio.mkdir(parents=True, exist_ok=True)
    files = []
    assets = [
        ("dialogue_44100.wav", 44100, 2.8, 0.70, _dialogue),
        ("music_96000.wav", 96000, 3.2, 0.60, _music),
        ("ambience_22050.wav", 22050, 1.3, 0.50, _ambience),
    ]
    for index, (name, rate, seconds, peak, builder) in enumerate(assets):
        data = _normalise(
            builder(rate, seconds, seed + 101 * (index + 1)), peak, rate
        )
        path = audio / name
        _write_audio(path, data, rate)
        files.append(path)
    manifest_path = root / "project.json"
    manifest_path.write_text(
        json.dumps(_manifest(seed), indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    return manifest_path, files

