"""Generate deterministic WAV fixtures plus ground-truth annotations."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from audiocut import wavio  # noqa: E402

SR = 48000


def synth_syllable(dur_s, rng, fade_in=0.012, fade_out=0.018):
    n = max(1, int(dur_s * SR))
    t = np.arange(n) / SR
    f0 = rng.uniform(95.0, 210.0)
    sig = np.zeros(n)
    for k in range(1, 6):
        sig += (1.0 / k) * np.sin(2 * np.pi * f0 * k * t + rng.uniform(0, 6.28))
    sig += 0.05 * rng.normal(0, 1, n)  # breathy component
    env = np.ones(n)
    ni, no = min(n // 2, int(fade_in * SR)), min(n // 2, int(fade_out * SR))
    if ni:
        env[:ni] = 0.5 - 0.5 * np.cos(np.pi * np.arange(ni) / ni)
    if no:
        env[-no:] = 0.5 + 0.5 * np.cos(np.pi * np.arange(no) / no)
    sig *= env
    return sig / max(np.sqrt(np.mean(sig ** 2)), 1e-12)


def drifting_noise(n, rng, lo=-72.0, hi=-54.0, level=None):
    if level is not None:
        db = np.full(n, level)
    else:
        steps = max(2, n // (4 * SR) + 1)  # slow random walk, <=3 dB per 4 s
        walk = rng.uniform(lo, hi) + np.cumsum(rng.uniform(-3, 3, steps))
        points = np.clip(walk, lo, hi)
        db = np.interp(np.arange(n), np.linspace(0, n, points.size), points)
    return rng.normal(0, 1, n) * (10 ** (db / 20.0))


def compose(rng, syllables, gaps, speech_db=-20.0, noise_kw=None, fades=None):
    """Place syllables after gaps; return (signal, truth segment list)."""
    noise_kw = noise_kw or {}
    pieces, truth, cursor = [], [], 0
    for i, (dur, gap) in enumerate(zip(syllables, gaps)):
        gap_n = int(gap * SR)
        pieces.append(np.zeros(gap_n))
        cursor += gap_n
        fi, fo = (fades[i] if fades else (0.012, 0.018))
        syl = synth_syllable(dur, rng, fi, fo) * (10 ** (speech_db / 20.0))
        pieces.append(syl)
        truth.append({"start_sample": cursor, "end_sample": cursor + syl.size})
        cursor += syl.size
    sig = np.concatenate(pieces) if pieces else np.zeros(SR)
    return sig + drifting_noise(sig.size, rng, **noise_kw), truth


def uniform(rng, lo, hi, n):
    return rng.uniform(lo, hi, n).tolist()


def build_fixtures(out: Path, seed: int):
    specs = []
    rng = np.random.default_rng(seed)

    def add(name, channels, bits, syllables, gaps, **kw):
        specs.append((name, channels, bits, syllables, gaps, kw))

    add("calib_a.wav", 1, 16, uniform(rng, .3, .5, 4), [2] + uniform(rng, 1.5, 3, 4))
    add("conv_01.wav", 1, 16, uniform(rng, .2, .6, 8), [3] + uniform(rng, 1.5, 3.5, 8))
    add("conv_02.wav", 2, 16, uniform(rng, .2, .6, 8), [3] + uniform(rng, 1.5, 3.5, 8))
    add("conv_03.wav", 1, 24, uniform(rng, .2, .6, 7), [3] + uniform(rng, 1.5, 3.5, 7))
    add("conv_04.wav", 2, 24, uniform(rng, .2, .6, 8), [3] + uniform(rng, 1.5, 3.5, 8))
    add("low_snr.wav", 1, 16, uniform(rng, .25, .5, 8), [3] + uniform(rng, 1, 2, 8),
        speech_db=-48.0, noise_kw={"level": -60.0})
    add("fades.wav", 1, 16, uniform(rng, .3, .5, 6), [3] + uniform(rng, 1, 2, 6),
        fades=[(f, f) for f in uniform(rng, .02, .2, 6)])
    add("short_syll.wav", 1, 16, uniform(rng, .12, .16, 10), [2] + uniform(rng, .4, .6, 10))
    add("dense.wav", 2, 16, uniform(rng, .15, .3, 12), [2] + uniform(rng, .25, .45, 12))
    add("sparse.wav", 1, 24, uniform(rng, .3, .6, 5), [4] + uniform(rng, 4, 6, 5))
    add("drift.wav", 1, 16, uniform(rng, .25, .5, 7), [3] + uniform(rng, 2, 4, 7),
        noise_kw={"lo": -72.0, "hi": -54.0})
    add("all_silence.wav", 1, 16, [], [20.0], noise_kw={"level": -66.0})
    add("all_speech.wav", 2, 16, uniform(rng, .7, .9, 10), [1] + [0.05] * 10)

    truth = {}
    for idx, (name, channels, bits, syllables, gaps, kw) in enumerate(specs):
        frng = np.random.default_rng(seed + idx + 1)
        fades = kw.pop("fades", None)
        sig, segs = compose(frng, syllables, gaps, fades=fades, **kw)
        wavio.write_wav(out / name, sig, SR, bits=bits, channels=channels)
        truth[name] = {"segments": segs, "channels": channels, "bits": bits,
                       "total_samples": int(sig.size)}
    (out / "truth.json").write_text(json.dumps(truth, indent=1))
    return truth


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", required=True)
    parser.add_argument("--seed", type=int, default=20261114)
    args = parser.parse_args(argv)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    truth = build_fixtures(out, args.seed)
    n_seg = sum(len(v["segments"]) for v in truth.values())
    print(json.dumps({"files": len(truth), "segments": n_seg, "out": str(out)}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
