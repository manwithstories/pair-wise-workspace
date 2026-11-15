"""Generate a deterministic, self-contained demo session."""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import soundfile as sf


def _time(rate: int, duration: float) -> np.ndarray:
    return np.arange(int(round(rate * duration)), dtype=np.float64) / rate


def _dialogue(rate: int, rng: np.random.Generator) -> np.ndarray:
    t = _time(rate, 5.4)
    syllables = 0.5 + 0.5 * np.sin(2.0 * np.pi * 2.2 * t) ** 2
    phrase = np.clip(np.sin(2.0 * np.pi * 0.17 * t) * 1.8 + 0.35, 0.0, 1.0)
    voiced = (
        0.31 * np.sin(2.0 * np.pi * 182.0 * t)
        + 0.15 * np.sin(2.0 * np.pi * 364.0 * t + 0.3)
        + 0.07 * np.sin(2.0 * np.pi * 721.0 * t + 1.1)
    )
    breath = rng.standard_normal(t.size) * 0.018
    return ((voiced + breath) * syllables * phrase * 1.55).astype(np.float32)


def _music(rate: int, rng: np.random.Generator) -> np.ndarray:
    t = _time(rate, 6.05)
    envelope = 0.72 + 0.28 * np.sin(2.0 * np.pi * 0.11 * t)
    left = 0.47 * np.sin(2.0 * np.pi * 110.0 * t) + 0.23 * np.sin(2.0 * np.pi * 164.8 * t)
    right = 0.47 * np.sin(2.0 * np.pi * 110.4 * t + 0.15) + 0.23 * np.sin(2.0 * np.pi * 220.0 * t)
    shimmer = 0.07 * np.sin(2.0 * np.pi * 880.0 * t + 0.4)
    pad = np.stack((left + shimmer, right + shimmer * 0.8), axis=1) * envelope[:, None]
    return (pad * 1.45 + rng.standard_normal(pad.shape) * 0.004).astype(np.float32)


def _effects(rate: int, rng: np.random.Generator) -> np.ndarray:
    t = _time(rate, 4.8)
    output = np.zeros((t.size, 2), dtype=np.float64)
    for start, length, frequency, gain in ((0.25, 0.55, 95.0, 0.7), (1.8, 0.22, 1300.0, 0.42), (3.2, 0.8, 58.0, 0.62)):
        i0, i1 = int(start * rate), min(t.size, int((start + length) * rate))
        local = t[i0:i1] - start
        attack = np.minimum(1.0, local / 0.018)
        decay = np.exp(-local * 3.2)
        wave = np.sin(2.0 * np.pi * frequency * local) + 0.25 * np.sin(2.0 * np.pi * frequency * 2.03 * local)
        output[i0:i1, 0] += gain * attack * decay * wave
        output[i0:i1, 1] += gain * attack * decay * wave * 0.88
    output += rng.standard_normal(output.shape) * 0.015
    output[rate : rate + 3, 1] = np.nan
    output[rate * 2 : rate * 2 + 2, 0] = np.inf
    return output.astype(np.float32)


def _foley(rate: int, rng: np.random.Generator) -> np.ndarray:
    t = _time(rate, 4.1)
    output = np.zeros(t.size, dtype=np.float64)
    for start in (0.4, 1.25, 2.6, 3.45):
        i0, i1 = int(start * rate), min(t.size, int((start + 0.18) * rate))
        local = t[i0:i1] - start
        click = np.exp(-local * 28.0) * np.sin(2.0 * np.pi * 520.0 * local)
        noise = rng.standard_normal(i1 - i0) * np.exp(-local * 40.0)
        output[i0:i1] += 0.52 * click + 0.33 * noise
    output += rng.standard_normal(t.size) * 0.07
    output += 0.05 * np.sin(2.0 * np.pi * 55.0 * t)
    return output.astype(np.float32)


def _write(path: Path, data: np.ndarray, rate: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(path), data, rate, subtype="FLOAT")


def _curve(curve_id: str, points: list, domain: str = "db") -> dict:
    return {"id": curve_id, "domain": domain, "points": points}


def build_graph() -> dict:
    return {
        "project": {"sample_rate": 48000, "channels": 2, "duration": 6.0},
        "metadata": {"title": "Neon Harbor - six person audio post demo", "seed": 7},
        "nodes": [
            {"id": "dialogue_src", "kind": "source", "source": {"path": "assets/dialogue.wav", "sample_rate": 32000, "channels": 1, "start_sec": 0.0137}},
            {"id": "music_src", "kind": "source", "source": {"path": "assets/music.wav", "sample_rate": 44100, "channels": 2, "start_sec": 0.0417}},
            {"id": "sfx_src", "kind": "source", "source": {"path": "assets/sfx.wav", "sample_rate": 48000, "channels": 2, "start_sec": 0.1731}},
            {"id": "foley_src", "kind": "source", "source": {"path": "assets/foley.wav", "sample_rate": 96000, "channels": 1, "start_sec": 0.1111}},
            {"id": "silent_src", "kind": "source", "source": {"path": "assets/silent.wav", "sample_rate": 48000, "channels": 2, "start_sec": 0.25}},
            {"id": "empty_src", "kind": "source", "source": {"path": "assets/empty.wav", "sample_rate": 48000, "channels": 1, "start_sec": 0.5}},
            {
                "id": "dialogue",
                "kind": "track",
                "inputs": ["dialogue_src"],
                "curves": [
                    _curve("dialogue_ride", [[0.0, -6.0, "linear"], [0.8, -4.0, "cosine"], [2.4, -7.0, "hold"], [5.8, -5.0, "linear"]]),
                    _curve("dialogue_detail", [[0.0, 1.0, "linear"], [2.0, 0.82, "cosine"], [5.9, 0.92, "linear"]], "linear"),
                ],
            },
            {
                "id": "music",
                "kind": "track",
                "inputs": ["music_src"],
                "curves": [
                    _curve("music_arc", [[0.0, -9.0, "linear"], [1.5, -7.5, "cosine"], [3.2, -9.5, "linear"], [5.8, -6.5, "hold"]]),
                    _curve("music_trim", [[0.0, 0.84, "linear"], [5.9, 0.84, "hold"]], "linear"),
                ],
            },
            {"id": "sfx", "kind": "track", "inputs": ["sfx_src"], "curves": [_curve("sfx_ride", [[0.0, -8.0, "linear"], [2.0, -5.0, "cosine"], [4.6, -7.0, "hold"]])]},
            {"id": "foley", "kind": "track", "inputs": ["foley_src"], "curves": [_curve("foley_shape", [[0.0, -7.0, "hold"], [1.0, -5.0, "cosine"], [4.0, -8.0, "linear"]])]},
            {"id": "silent_ambience", "kind": "track", "inputs": ["silent_src"], "gain_db": -3.0},
            {"id": "empty_cue", "kind": "track", "inputs": ["empty_src"], "gain_db": -3.0},
            {"id": "dialogue_bus", "kind": "bus", "inputs": ["dialogue"], "curves": [_curve("dialogue_bus_trim", [[0.0, -1.5, "linear"], [5.9, -2.0, "hold"]])]},
            {
                "id": "music_bus",
                "kind": "bus",
                "inputs": ["music"],
                "curves": [_curve("music_bus_tone", [[0.0, -1.0, "linear"], [5.9, -1.5, "hold"]])],
                "sidechain": {"source": "dialogue", "attack_sec": 0.012, "release_sec": 0.18, "max_duck_db": 8.0, "threshold_db": -30.0},
            },
            {"id": "sfx_bus", "kind": "bus", "inputs": ["sfx"], "curves": [_curve("sfx_bus_air", [[0.0, -1.0, "linear"], [5.9, -1.0, "hold"]])]},
            {"id": "foley_bus", "kind": "bus", "inputs": ["foley"], "curves": [_curve("foley_bus_trim", [[0.0, -1.0, "hold"], [5.9, -1.0, "hold"]])]},
            {"id": "master", "kind": "bus", "inputs": ["dialogue_bus", "music_bus", "sfx_bus", "foley_bus"], "curves": [_curve("master_safe", [[0.0, -3.0, "linear"], [5.9, -3.0, "hold"]])]},
        ],
        "outputs": {
            "master": "master",
            "tracks": ["dialogue", "music", "sfx", "foley", "silent_ambience", "empty_cue"],
            "buses": ["dialogue_bus", "music_bus", "sfx_bus", "foley_bus"],
        },
    }


def generate_fixtures(out_dir: str | Path, seed: int = 7) -> dict:
    output = Path(out_dir)
    assets = output / "assets"
    assets.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(seed)
    _write(assets / "dialogue.wav", _dialogue(32000, rng), 32000)
    _write(assets / "music.wav", _music(44100, rng), 44100)
    _write(assets / "sfx.wav", _effects(48000, rng), 48000)
    _write(assets / "foley.wav", _foley(96000, rng), 96000)
    _write(assets / "silent.wav", np.zeros((96000, 2), dtype=np.float32), 48000)
    _write(assets / "empty.wav", np.zeros((0, 1), dtype=np.float32), 48000)
    graph = build_graph()
    graph["metadata"]["seed"] = seed
    graph_path = output / "demo.mix.json"
    with graph_path.open("w", encoding="utf-8") as handle:
        json.dump(graph, handle, indent=2, ensure_ascii=False)
        handle.write("\n")
    return {"graph": str(graph_path), "assets": str(assets), "seed": seed}
