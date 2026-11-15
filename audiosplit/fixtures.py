"""Deterministic synthetic masters with known segment truth."""

from __future__ import annotations

import json
import os
from pathlib import Path
import tempfile

import numpy as np

from .wavio import write_wav_atomic


SAMPLE_RATE = 48000
LETTERS = "abcdef"
SEGMENT_COUNTS = (5, 6, 5, 5, 5, 6)
NOISE_TARGETS = (-55.0, -54.0, -53.0, -52.0, -56.0, -51.0)


def _seconds_to_samples(seconds: float) -> int:
    return int(round(seconds * SAMPLE_RATE))


def _write_json_atomic(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fp:
            json.dump(payload, fp, ensure_ascii=False, indent=2)
            fp.write("\n")
            fp.flush()
            os.fsync(fp.fileno())
        os.replace(name, path)
    except Exception:
        try:
            os.unlink(name)
        except OSError:
            pass
        raise


def _speech(rng: np.random.Generator, duration: float, dc_offset: float) -> np.ndarray:
    count = _seconds_to_samples(duration)
    time = np.arange(count, dtype=np.float64) / SAMPLE_RATE
    base = rng.uniform(185.0, 255.0)
    vibrato = 1.0 + 0.012 * np.sin(2.0 * np.pi * 4.7 * time + rng.uniform(0, 2 * np.pi))
    phase = 2.0 * np.pi * np.cumsum(base * vibrato) / SAMPLE_RATE
    voiced = (
        0.20 * np.sin(phase)
        + 0.075 * np.sin(2.0 * phase + 0.3)
        + 0.035 * np.sin(3.0 * phase + 0.7)
    )
    voiced *= 0.82 + 0.18 * np.sin(2.0 * np.pi * 5.3 * time)
    fricative = rng.normal(0.0, 0.035, count)
    fricative *= np.clip((time - duration * 0.72) / (duration * 0.28), 0.0, 1.0)
    burst_len = min(count, _seconds_to_samples(0.009))
    burst = np.zeros(count)
    if burst_len:
        burst[:burst_len] = rng.normal(0.0, 0.55, burst_len) * np.exp(
            -np.arange(burst_len) / max(1.0, SAMPLE_RATE * 0.0025)
        )
    fade_in = np.clip(time / 0.010, 0.0, 1.0)
    fade_out = np.clip((duration - time) / 0.004, 0.0, 1.0)
    envelope = 0.5 - 0.5 * np.cos(np.pi * np.minimum(fade_in, fade_out))
    return ((voiced + fricative + burst) * envelope + dc_offset).astype(np.float32)


def _breath(rng: np.random.Generator, count: int, level_dbfs: float) -> np.ndarray:
    noise = rng.normal(0.0, 1.0, count)
    noise *= 10.0 ** (level_dbfs / 20.0) / max(np.sqrt(np.mean(noise**2)), 1e-12)
    window = np.hanning(count) if count > 2 else np.ones(count)
    return (noise * window).astype(np.float32)


def _make_master(seed: int, index: int) -> tuple[np.ndarray, list[tuple[int, int]]]:
    rng = np.random.default_rng(seed + index * 1009)
    count = SEGMENT_COUNTS[index]
    lead = _seconds_to_samples(rng.uniform(0.20, 0.28))
    trail = _seconds_to_samples(rng.uniform(0.26, 0.34))
    durations = [_seconds_to_samples(rng.uniform(0.58, 0.82)) for _ in range(count)]
    gaps = [_seconds_to_samples(rng.uniform(0.36, 0.44)) for _ in range(count + 1)]
    cursor = lead
    truth: list[tuple[int, int]] = []
    for duration in durations:
        truth.append((cursor, cursor + duration))
        cursor += duration + gaps[len(truth)]
    total = truth[-1][1] + trail

    base_noise = rng.normal(0.0, 1.0, total)
    target = 10.0 ** (NOISE_TARGETS[index] / 20.0)
    base_noise *= target / max(np.sqrt(np.mean(base_noise**2)), 1e-12)
    time = np.arange(total) / SAMPLE_RATE
    drift = 10.0 ** (1.1 * np.sin(2.0 * np.pi * 0.17 * time + rng.uniform(0, 2 * np.pi)) / 20.0)
    master = (base_noise * drift).astype(np.float32)
    dc_offset = 0.004 if index == 3 else 0.0
    for segment_index, (start, end) in enumerate(truth):
        speech = _speech(rng, (end - start) / SAMPLE_RATE, dc_offset)
        master[start:end] += speech[: end - start]
        if segment_index + 1 < count:
            gap_start = end
            gap_end = truth[segment_index + 1][0]
            breath_len = max(0, min(_seconds_to_samples(0.12), gap_end - gap_start))
            if breath_len:
                center = gap_start + (gap_end - gap_start - breath_len) // 2
                master[center : center + breath_len] += _breath(rng, breath_len, NOISE_TARGETS[index] + 1.5)
    master = np.clip(master, -0.98, 0.98)
    return master.astype(np.float32), truth


def generate_fixtures(out: str | os.PathLike[str], seed: int = 7, files: int = 6) -> dict:
    if files < 1:
        raise ValueError("files must be positive")
    target = Path(out)
    target.mkdir(parents=True, exist_ok=True)
    truth_files = []
    for index in range(files):
        letter = LETTERS[index % len(LETTERS)]
        suffix = "" if index < len(LETTERS) else f"_{index + 1}"
        name = f"take_{letter}{suffix}.wav"
        samples, truth = _make_master(seed, index)
        write_wav_atomic(target / name, SAMPLE_RATE, samples)
        truth_files.append(
            {
                "file": name,
                "sample_rate": SAMPLE_RATE,
                "segments": [
                    {"start_sample": start, "end_sample": end} for start, end in truth
                ],
            }
        )
    manifest = {
        "version": 1,
        "seed": seed,
        "sample_rate": SAMPLE_RATE,
        "files": truth_files,
    }
    _write_json_atomic(target / "truth.json", manifest)
    return {
        "out": str(target),
        "seed": seed,
        "files": [item["file"] for item in truth_files],
        "file_count": len(truth_files),
        "segment_count": sum(len(item["segments"]) for item in truth_files),
        "truth_manifest": str(target / "truth.json"),
    }

