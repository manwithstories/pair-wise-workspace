"""Generate deterministic multi-rate audio and a demo mix graph."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path

import numpy as np
import soundfile as sf


def _stereo(left: np.ndarray, right: np.ndarray | None = None) -> np.ndarray:
    if right is None:
        right = left
    return np.column_stack((left, right)).astype(np.float64)


def _dialogue(sample_rate: int, duration: float) -> np.ndarray:
    t = np.arange(int(round(sample_rate * duration)), dtype=np.float64) / sample_rate
    phrase = np.maximum(0.0, np.sin(2 * np.pi * 1.15 * t + 0.4)) ** 0.35
    phrase *= np.clip(np.sin(2 * np.pi * 0.17 * t) + 0.75, 0.0, 1.0)
    voice = (
        0.23 * np.sin(2 * np.pi * 185.0 * t)
        + 0.045 * np.sin(2 * np.pi * 370.0 * t + 0.3)
        + 0.018 * np.sin(2 * np.pi * 555.0 * t + 0.7)
    )
    left = phrase * voice
    right = phrase * 0.97 * (
        0.23 * np.sin(2 * np.pi * 185.0 * t + 0.015)
        + 0.045 * np.sin(2 * np.pi * 370.0 * t + 0.33)
    )
    return _stereo(left, right)


def _music(sample_rate: int, duration: float) -> np.ndarray:
    t = np.arange(int(round(sample_rate * duration)), dtype=np.float64) / sample_rate
    chord = np.zeros_like(t)
    for index, frequency in enumerate((110.0, 164.81, 220.0, 329.63)):
        chord += (0.060 - index * 0.006) * np.sin(
            2 * np.pi * frequency * t + index * 0.35
        )
    breathe = 0.78 + 0.22 * np.sin(2 * np.pi * 0.09 * t)
    left = chord * breathe
    right = np.roll(chord, int(round(0.003 * sample_rate))) * (0.93 + 0.07 * breathe)
    return _stereo(left, right)


def _sfx(sample_rate: int, duration: float) -> np.ndarray:
    frames = int(round(sample_rate * duration))
    rng = np.random.default_rng(10954)
    output = np.zeros(frames, dtype=np.float64)
    for onset, level, decay in ((0.65, 0.36, 0.10), (1.92, 0.50, 0.07),
                                (3.05, 0.55, 0.12), (4.38, 0.42, 0.08),
                                (5.41, 0.48, 0.06)):
        start = int(round(onset * sample_rate))
        length = min(frames - start, int(round(0.5 * sample_rate)))
        if length <= 0:
            continue
        local = np.arange(length, dtype=np.float64) / sample_rate
        burst = rng.normal(0.0, 1.0, length) * np.exp(-local / decay)
        burst += 0.8 * np.sin(2 * np.pi * 72.0 * local) * np.exp(-local / 0.16)
        burst *= level / max(np.max(np.abs(burst)), 1e-12)
        output[start : start + length] += burst
    side = output * 0.82
    return _stereo(output, side)


def _ambience(sample_rate: int, duration: float) -> np.ndarray:
    frames = int(round(sample_rate * duration))
    t = np.arange(frames, dtype=np.float64) / sample_rate
    rng = np.random.default_rng(44100)
    noise = rng.normal(0.0, 1.0, frames)
    kernel = np.ones(97, dtype=np.float64) / 97.0
    noise = np.convolve(noise, kernel, mode="same")
    noise *= 0.115 / max(np.max(np.abs(noise)), 1e-12)
    tone = 0.018 * np.sin(2 * np.pi * 61.0 * t)
    left = noise + tone
    right = np.roll(noise, 43) + tone * 0.9
    return _stereo(left, right)


def _write_csv(path: Path, points: list[tuple[float, float]]) -> None:
    with path.open("w", encoding="utf-8", newline="") as handle:
        handle.write("time_sec,gain_db\n")
        for time_sec, gain_db in points:
            handle.write(f"{time_sec:.6f},{gain_db:.6f}\n")


def _graph_path(path: Path, workspace: Path) -> str:
    try:
        return path.resolve().relative_to(workspace.resolve()).as_posix()
    except ValueError:
        return os.path.relpath(path.resolve(), workspace.resolve())


def generate_fixtures(out_dir: str | Path, duration: float = 6.0) -> dict[str, object]:
    if duration <= 0:
        raise ValueError("duration must be positive")
    workspace = Path.cwd().resolve()
    out = Path(out_dir)
    if not out.is_absolute():
        out = workspace / out
    out = out.resolve()
    out.mkdir(parents=True, exist_ok=True)

    tracks = (
        ("dialogue", 44_100, _dialogue, [(-5.0, 0.0), (-3.5, 1.4), (-6.0, 3.2),
                                         (-4.5, 4.8), (-5.5, duration)], 0.16),
        ("music", 48_000, _music, [(-6.0, 0.0), (-4.5, 1.8), (-6.5, 3.3),
                                    (-5.0, 4.7), (-6.0, duration)], -0.22),
        ("sfx", 32_000, _sfx, [(-4.5, 0.0), (-3.0, 1.9), (-5.0, 3.05),
                                (-3.5, 4.4), (-4.0, duration)], 0.31),
        ("ambience", 44_100, _ambience, [(-2.0, 0.0), (-1.0, 2.0), (-2.5, 3.8),
                                          (-1.5, duration)], -0.38),
    )
    nodes: list[dict[str, object]] = []
    stems: list[str] = []
    track_sum_inputs: list[str] = []
    for name, sample_rate, generator, points, pan in tracks:
        data = generator(sample_rate, duration)
        wav_path = out / f"{name}_{sample_rate // 1000}k.wav"
        sf.write(str(wav_path), data, sample_rate, subtype="PCM_24")
        stem = _graph_path(wav_path, workspace)
        curve_path = out / f"{name}_gain.csv"
        _write_csv(curve_path, [(time, gain) for gain, time in points])
        nodes.extend(
            [
                {"id": name, "type": "source", "path": stem},
                {
                    "id": f"{name}_gain",
                    "type": "gain",
                    "input": name,
                    "automation": _graph_path(curve_path, workspace),
                },
                {"id": f"{name}_pan", "type": "pan", "input": f"{name}_gain",
                 "pan": pan},
            ]
        )
        stems.append(stem)
        track_sum_inputs.append(f"{name}_pan")
    nodes.extend(
        [
            {"id": "mix_bus", "type": "sum", "inputs": track_sum_inputs},
            {"id": "master", "type": "limiter", "input": "mix_bus",
             "ceiling_db": -1.0},
        ]
    )
    graph = {
        "schema": "mix-graph/1",
        "master_sr": 48_000,
        "block_size": 1024,
        "output": "master",
        "nodes": nodes,
    }
    graph_path = out / "session.json"
    graph_path.write_text(json.dumps(graph, indent=2) + "\n", encoding="utf-8")
    return {
        "out_dir": str(out),
        "graph": str(graph_path),
        "stems": stems,
        "duration": duration,
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate mixer demo fixtures")
    parser.add_argument("--out-dir", default="fixtures")
    parser.add_argument("--duration", type=float, default=6.0)
    args = parser.parse_args(argv)
    result = generate_fixtures(args.out_dir, args.duration)
    print(json.dumps(result, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
