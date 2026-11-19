#!/usr/bin/env python3
"""Generate sample multicam material and timeline manifests.

Creates in --out:
  cam_a.wav       48 kHz stereo, 2 s, 440 Hz tone
  cam_b.wav       44.1 kHz mono, 1.1 s, 550 Hz tone (starts 0.1 s in)
  show.json       valid timeline: two tracks, overlap, 2 s delivery
  truncated.wav   cam_a.wav with the data chunk cut short
  truncated.json  same timeline pointing at the truncated file

Usage: python scripts/make_samples.py --out .tmp/samples
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

import numpy as np
import soundfile as sf

from stageflow.clock import MASTER_RATE

SECONDS = 2.0
DELIVERY_TICKS = int(SECONDS * MASTER_RATE)
OFFSET_B_TICKS = MASTER_RATE // 10  # cam_b starts 0.1 s into the timeline


def tone(rate: int, seconds: float, freq: float, channels: int) -> np.ndarray:
    t = np.arange(int(rate * seconds), dtype=np.float64) / rate
    x = 0.25 * np.sin(2 * np.pi * freq * t)
    return np.repeat(x[:, None], channels, axis=1).astype(np.float32)


def make_timeline(cam_a: str, cam_b: str) -> dict:
    return {
        "job": "show",
        "master_tick_rate": MASTER_RATE,
        "delivery": {
            "sample_rate": 48000,
            "channels": 2,
            "fps": 30,
            "width": 320,
            "height": 180,
            "start_tick": 0,
            "duration_ticks": DELIVERY_TICKS,
            "tracks": ["cam_a", "cam_b"],
        },
        "tracks": [
            {
                "id": "cam_a",
                "path": cam_a,
                "offset_ticks": 0,
                "segments": [{"start_tick": 0, "duration_ticks": DELIVERY_TICKS}],
                "cut_ticks": [MASTER_RATE // 2, MASTER_RATE],
            },
            {
                "id": "cam_b",
                "path": cam_b,
                "offset_ticks": OFFSET_B_TICKS,
                "segments": [
                    {"start_tick": OFFSET_B_TICKS, "duration_ticks": MASTER_RATE}
                ],
            },
        ],
    }


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", required=True, help="output directory for samples")
    args = ap.parse_args()
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    sf.write(out / "cam_a.wav", tone(48000, SECONDS, 440.0, 2), 48000, subtype="FLOAT")
    sf.write(out / "cam_b.wav", tone(44100, 1.1, 550.0, 1), 44100, subtype="FLOAT")

    (out / "show.json").write_text(
        json.dumps(make_timeline("cam_a.wav", "cam_b.wav"), indent=2) + "\n",
        encoding="utf-8",
    )

    # Truncated variant: valid header, data chunk cut ~20% short.
    data = (out / "cam_a.wav").read_bytes()
    (out / "truncated.wav").write_bytes(data[: int(len(data) * 0.8)])
    bad = make_timeline("truncated.wav", "cam_b.wav")
    bad["job"] = "truncated"
    (out / "truncated.json").write_text(
        json.dumps(bad, indent=2) + "\n", encoding="utf-8"
    )

    print(f"wrote samples to {out}")


if __name__ == "__main__":
    main()
