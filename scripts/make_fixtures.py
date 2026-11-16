"""Generate deterministic WAV fixtures: noise beds, fades, varied gaps."""
from __future__ import annotations

import argparse
import wave
from pathlib import Path

import numpy as np

SEED = 11775


def write_wav(path, samples, sample_rate, sampwidth=2, channels=1):
    data = np.asarray(samples, dtype=np.float32)
    if channels == 2:
        if data.ndim == 1:
            data = np.stack([data, data * 0.9], axis=1)
        data = data.reshape(-1)
    clipped = np.clip(data, -1.0, 1.0)
    if sampwidth == 2:
        raw = np.round(clipped * 32767.0).astype("<i2").tobytes()
    elif sampwidth == 3:
        ints = np.round(clipped * 8388607.0).astype("<i4")
        raw = np.frombuffer(ints.tobytes(), np.uint8).reshape(-1, 4)[:, :3].tobytes()
    elif sampwidth == 4:
        raw = np.round(clipped * 2147483647.0).astype("<i4").tobytes()
    else:
        raise ValueError(f"unsupported sampwidth {sampwidth}")
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as out:
        out.setnchannels(channels)
        out.setsampwidth(sampwidth)
        out.setframerate(sample_rate)
        out.writeframes(raw)


def make_event(rng, sample_rate, dur_ms, fade_in_ms, fade_out_ms,
               level_db=-14.0):
    count = int(sample_rate * dur_ms / 1000.0)
    time = np.arange(count) / sample_rate
    freq = rng.uniform(200.0, 4000.0)
    tone = np.sin(2.0 * np.pi * freq * time + rng.uniform(0.0, 6.2832))
    mod = 0.6 + 0.4 * np.sin(2.0 * np.pi * rng.uniform(3.0, 30.0) * time)
    env = np.ones(count)
    fade_in = min(count // 2, int(sample_rate * fade_in_ms / 1000.0))
    fade_out = min(count // 2, int(sample_rate * fade_out_ms / 1000.0))
    if fade_in:
        env[:fade_in] = 0.5 * (1.0 - np.cos(np.pi * np.arange(fade_in) / fade_in))
    if fade_out:
        env[-fade_out:] = 0.5 * (1.0 + np.cos(np.pi * np.arange(fade_out) / fade_out))
    event = tone * mod * env
    event *= (10.0 ** (level_db / 20.0)) / max(np.abs(event).max(), 1e-9)
    return event.astype(np.float32)


def assemble_pack(rng, sample_rate, event_specs, noise_db, head_ms, tail_ms):
    """Place events on a continuous noise bed, separated by gaps."""
    head = int(sample_rate * head_ms / 1000.0)
    tail = int(sample_rate * tail_ms / 1000.0)
    total = head + tail + sum(len(ev) + gap for ev, gap in event_specs)
    bed = (rng.randn(total).astype(np.float32)
           * float(10.0 ** (noise_db / 20.0)))
    cursor = head
    for event, gap in event_specs:
        bed[cursor:cursor + len(event)] += event
        cursor += len(event) + gap
    return bed


def random_specs(rng, sample_rate, count, dur_range, gap_range, fade_range):
    specs = []
    for _ in range(count):
        dur = rng.uniform(*dur_range)
        gap = int(sample_rate * rng.uniform(*gap_range) / 1000.0)
        fade_in = rng.uniform(*fade_range)
        fade_out = rng.uniform(*fade_range)
        specs.append((make_event(rng, sample_rate, dur, fade_in, fade_out), gap))
    return specs


def generate_all(out_dir):
    out = Path(out_dir)
    rng = np.random.RandomState(SEED)

    specs = random_specs(rng, 48000, 18, (200.0, 900.0), (250.0, 900.0),
                         (5.0, 60.0))
    pack = assemble_pack(rng, 48000, specs, -58.0, 150.0, 200.0)
    write_wav(out / "pack_noisy_fade.wav", pack, 48000, sampwidth=2)

    batch = out / "batch"
    specs = random_specs(rng, 44100, 6, (250.0, 700.0), (300.0, 900.0),
                         (10.0, 80.0))
    write_wav(batch / "alpha_16_stereo.wav",
              assemble_pack(rng, 44100, specs, -60.0, 150.0, 150.0),
              44100, sampwidth=2, channels=2)

    specs = random_specs(rng, 48000, 5, (300.0, 800.0), (300.0, 800.0),
                         (5.0, 40.0))
    write_wav(batch / "bravo_24bit.wav",
              assemble_pack(rng, 48000, specs, -55.0, 150.0, 150.0),
              48000, sampwidth=3)

    specs = random_specs(rng, 48000, 4, (200.0, 600.0), (400.0, 900.0),
                         (10.0, 50.0))
    write_wav(batch / "charlie_32bit.wav",
              assemble_pack(rng, 48000, specs, -65.0, 150.0, 150.0),
              48000, sampwidth=4)

    specs = random_specs(rng, 44100, 5, (300.0, 900.0), (300.0, 700.0),
                         (150.0, 300.0))
    write_wav(batch / "nested" / "delta_fades.wav",
              assemble_pack(rng, 44100, specs, -58.0, 200.0, 200.0),
              44100, sampwidth=2, channels=2)
    return out


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, help="fixture output directory")
    args = parser.parse_args(argv)
    out = generate_all(args.out)
    print(f"fixtures written to {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
