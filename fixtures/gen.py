"""Generate deterministic fixture WAVs with a fixed seed.

The fixture mimics a museum guide take: speech-like tone-burst syllables
with 60-180 ms fade-in/out ramps per segment, short inner pauses (below
the min-silence threshold, exercising gap bridging), longer inter-exhibit
silences, and a slowly drifting noise floor around -50 dBFS. Everything
is synthesised locally; no files outside the repo are referenced.

Usage: python3 fixtures/gen.py [--out fixtures/line_a.wav] [--seed N]
"""
from __future__ import annotations

import argparse
import os
import struct

import numpy as np

SR = 44100
SEED = 20261115
N_SEGMENTS = 7


def _write_wav_pcm16(path: str, samples: np.ndarray) -> None:
    pcm16 = (np.clip(samples, -1.0, 1.0) * 32767.0).astype("<i2")
    data = pcm16.tobytes()
    header = (b"RIFF" + struct.pack("<I", 36 + len(data)) + b"WAVE"
              + b"fmt "
              + struct.pack("<IHHIIHH", 16, 1, 1, SR, SR * 2, 2, 16)
              + b"data" + struct.pack("<I", len(data)))
    with open(path, "wb") as fh:
        fh.write(header + data)


def _speech(rng: np.random.Generator, dur_s: float) -> np.ndarray:
    n = int(dur_s * SR)
    out = np.zeros(n, dtype=np.float64)
    pos = 0
    while pos < n - int(0.15 * SR):
        syll = min(int(rng.uniform(0.15, 0.42) * SR), n - pos)
        f0 = rng.uniform(110.0, 240.0)
        t = np.arange(syll) / SR
        amp = rng.uniform(0.15, 0.30)
        tone = (np.sin(2 * np.pi * f0 * t)
                + 0.5 * np.sin(2 * np.pi * 2 * f0 * t)
                + 0.25 * np.sin(2 * np.pi * 3 * f0 * t))
        env = np.ones(syll)
        atk = max(1, int(0.03 * SR))
        rel = max(1, int(0.04 * SR))
        env[:atk] = np.linspace(0.0, 1.0, atk)
        env[-rel:] = np.linspace(1.0, 0.0, rel)
        out[pos:pos + syll] += tone * env * (amp / 1.75)
        pos += syll + int(rng.uniform(0.08, 0.26) * SR)  # inner pause
    fade_in = int(rng.uniform(0.06, 0.18) * SR)
    fade_out = int(rng.uniform(0.06, 0.18) * SR)
    out[:fade_in] *= np.linspace(0.0, 1.0, fade_in)
    out[-fade_out:] *= np.linspace(1.0, 0.0, fade_out)
    return out


def _drifting_noise(rng: np.random.Generator, n: int) -> np.ndarray:
    walk = np.cumsum(rng.normal(0.0, 0.15, n))
    kernel = np.ones(SR) / SR  # 1 s box smoothing
    walk = np.convolve(walk, kernel, mode="same")
    walk -= walk.mean()
    level_db = -50.0 + np.clip(walk, -4.0, 4.0)  # -54 .. -46 dBFS
    return rng.normal(0.0, 1.0, n) * (10.0 ** (level_db / 20.0))


def generate(path: str, seed: int = SEED) -> dict:
    rng = np.random.default_rng(seed)
    head_s, tail_s = 2.0, 2.5
    seg_durs = rng.uniform(3.0, 6.0, size=N_SEGMENTS)
    gap_durs = rng.uniform(1.0, 2.2, size=N_SEGMENTS - 1)
    total_s = head_s + tail_s + seg_durs.sum() + gap_durs.sum()
    n = int(total_s * SR)
    signal = _drifting_noise(rng, n)
    pos = int(head_s * SR)
    for i, dur in enumerate(seg_durs):
        seg = _speech(rng, float(dur))
        signal[pos:pos + seg.shape[0]] += seg
        pos += seg.shape[0]
        if i < len(gap_durs):
            pos += int(gap_durs[i] * SR)
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    _write_wav_pcm16(path, signal)
    return {"path": path, "sample_rate": SR, "samples": n,
            "segments": N_SEGMENTS, "seed": seed}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", default=os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "line_a.wav"))
    parser.add_argument("--seed", type=int, default=SEED)
    args = parser.parse_args()
    info = generate(args.out, args.seed)
    print(f"wrote {info['path']}: {info['samples']} samples, "
          f"{info['segments']} synthesised segments, seed {info['seed']}")


if __name__ == "__main__":
    main()
