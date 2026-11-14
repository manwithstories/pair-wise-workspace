#!/usr/bin/env python3
"""Generate deterministic multi-rate WAV fixtures and a declarative mix manifest."""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf


def _pick_rate(rates: list[int], desired: int) -> int:
    if desired in rates:
        return desired
    return min(rates, key=lambda value: abs(value - desired))


def _sine(rate: int, seconds: float, frequency: float, amplitude: float = 0.2) -> np.ndarray:
    time = np.arange(int(round(rate * seconds)), dtype=np.float64) / rate
    return (amplitude * np.sin(2.0 * np.pi * frequency * time)).astype(np.float32)


def _write(path: Path, data: np.ndarray, rate: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(path, data, rate, format="WAV", subtype="FLOAT")


def _track_nodes(track_id: str) -> list[dict[str, Any]]:
    read_id = f"{track_id}.read"
    gain_id = f"{track_id}.gain"
    pan_id = f"{track_id}.pan"
    return [
        {"id": read_id, "type": "read", "track": track_id},
        {"id": gain_id, "type": "gain", "track": track_id, "input": read_id},
        {"id": pan_id, "type": "pan", "track": track_id, "input": gain_id},
    ]


def make_fixtures(root: Path, rates: list[int], duration: float, seed: int) -> dict[str, Any]:
    if duration <= 0.0:
        raise ValueError("duration must be positive")
    rates = sorted(set(rates))
    root.mkdir(parents=True, exist_ok=True)
    audio_dir = root / "audio"
    rng = np.random.default_rng(seed)

    host_rate = _pick_rate(rates, 44100)
    guest_rate = _pick_rate(rates, 48000)
    music_rate = _pick_rate(rates, 48000)
    sfx_rate = _pick_rate(rates, 96000)
    room_rate = _pick_rate(rates, 48000)
    spot_rate = _pick_rate(rates, 48000)
    announcer_rate = _pick_rate(rates, 44100)
    field_rate = _pick_rate(rates, 96000)

    host = _sine(host_rate, 9.0, 220.0, 0.20)
    guest = _sine(guest_rate, 8.0, 330.0, 0.18)
    left = _sine(music_rate, 13.0, 110.0, 0.07)
    right = _sine(music_rate, 13.0, 111.5, 0.07)
    music = np.column_stack((left, right))
    sfx = np.zeros(int(round(sfx_rate * 4.0)), dtype=np.float32)
    sfx[: int(round(sfx_rate * 0.35))] = 1.0
    room = np.zeros(int(round(room_rate * duration)), dtype=np.float32)
    spot = np.zeros(0, dtype=np.float32)
    announcer_t = np.arange(int(round(announcer_rate * 7.0)), dtype=np.float64) / announcer_rate
    announcer = np.column_stack(
        (
            0.11 * np.sin(2.0 * np.pi * 180.0 * announcer_t),
            0.09 * np.sin(2.0 * np.pi * 185.0 * announcer_t),
        )
    ).astype(np.float32)
    field = (0.025 * rng.standard_normal((int(round(field_rate * 6.0)), 2))).astype(np.float32)

    files = {
        "host": ("audio/host_44k.wav", host_rate, host),
        "guest": ("audio/guest_48k.wav", guest_rate, guest),
        "music": ("audio/music_48k.wav", music_rate, music),
        "sfx": ("audio/sfx_96k.wav", sfx_rate, sfx),
        "room": ("audio/room_silence.wav", room_rate, room),
        "spot": ("audio/spot_empty.wav", spot_rate, spot),
        "announcer": ("audio/announcer_44k.wav", announcer_rate, announcer),
        "field": ("audio/field_96k.wav", field_rate, field),
    }
    for name, (relative, rate, data) in files.items():
        _write(root / relative, data, rate)

    tracks = [
        {
            "id": "host",
            "file": files["host"][0],
            "gain_db": -3.0,
            "pan": -0.20,
            "automation": [
                {
                    "type": "linear",
                    "start": 0.0,
                    "end": duration / 3.0,
                    "from_db": -6.0,
                    "to_db": -2.5,
                },
                {
                    "type": "exponential",
                    "start": duration / 3.0,
                    "end": duration * 2.0 / 3.0,
                    "from_db": -2.5,
                    "to_db": -12.0,
                },
                {
                    "type": "hold",
                    "start": duration * 2.0 / 3.0,
                    "end": duration,
                    "from_db": -12.0,
                    "to_db": -12.0,
                },
            ],
        },
        {
            "id": "guest",
            "file": files["guest"][0],
            "gain_db": -5.0,
            "pan": 0.30,
            "automation": [
                {"type": "hold", "start": 0.0, "end": duration, "from_db": -6.0, "to_db": -6.0}
            ],
        },
        {
            "id": "music",
            "file": files["music"][0],
            "gain_db": -10.0,
            "pan": 0.0,
            "automation": [
                {"type": "linear", "start": 0.0, "end": duration, "from_db": -18.0, "to_db": -9.0}
            ],
        },
        {"id": "sfx", "file": files["sfx"][0], "gain_db": 0.0, "pan": 0.40},
        {"id": "room", "file": files["room"][0], "gain_db": 0.0, "pan": 0.0},
        {"id": "spot", "file": files["spot"][0], "gain_db": 0.0, "pan": 0.0},
        {"id": "announcer", "file": files["announcer"][0], "gain_db": -8.0, "pan": -0.50},
        {"id": "field", "file": files["field"][0], "gain_db": -12.0, "pan": 0.70},
    ]
    nodes: list[dict[str, Any]] = []
    for track in tracks:
        nodes.extend(_track_nodes(track["id"]))
    sum_inputs = {f"in{index}": f"{track['id']}.pan" for index, track in enumerate(tracks)}
    nodes.extend(
        [
            {"id": "mix.sum", "type": "sum", "inputs": sum_inputs},
            {"id": "mix.gain", "type": "gain", "gain_db": 0.0, "input": "mix.sum"},
            {"id": "mix.trim", "type": "trim", "max_samples": int(round(duration * 48000)), "input": "mix.gain"},
            {
                "id": "mix.limiter",
                "type": "limiter",
                "ceiling": 0.98,
                "softness": 1.0,
                "input": "mix.trim",
            },
        ]
    )
    manifest = {
        "version": 1,
        "sample_rate": 48000,
        "channels": 2,
        "duration_s": duration,
        "buses": [{"id": "mix", "gain_db": 0.0, "ceiling": 0.98, "softness": 1.0}],
        "tracks": tracks,
        "nodes": nodes,
        "output_node": "mix.limiter",
    }
    manifest_path = root / "mix.json"
    manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return {
        "ok": True,
        "root": str(root),
        "manifest": str(manifest_path),
        "rates": rates,
        "tracks": len(tracks),
        "duration_s": duration,
        "seed": seed,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate local mixer fixtures")
    parser.add_argument("--root", default="fixtures/show")
    parser.add_argument("--rates", default="44100,48000,96000")
    parser.add_argument("--duration", type=float, default=12.0)
    parser.add_argument("--seed", type=int, default=20261115)
    args = parser.parse_args()
    try:
        rates = [int(value.strip()) for value in args.rates.split(",") if value.strip()]
    except ValueError as exc:
        raise SystemExit(f"invalid --rates: {exc}")
    if not rates or any(rate <= 0 for rate in rates):
        raise SystemExit("--rates requires positive integer sample rates")
    if not math.isfinite(args.duration) or args.duration <= 0.0:
        raise SystemExit("--duration must be a positive finite number")
    summary = make_fixtures(Path(args.root), rates, args.duration, args.seed)
    print(json.dumps(summary, ensure_ascii=False, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
