"""Export single-event 16-bit mono WAVs with fixed head/tail padding."""
from __future__ import annotations

import json
import wave
from pathlib import Path

import numpy as np

from . import wavio
from .store import load_run

DEFAULT_PAD_MS = 50.0


def _write_wav16(path, samples, sample_rate):
    clipped = np.clip(samples, -1.0, 1.0)
    pcm = np.round(clipped * 32767.0).astype("<i2")
    with wave.open(str(path), "wb") as out:
        out.setnchannels(1)
        out.setsampwidth(2)
        out.setframerate(sample_rate)
        out.writeframes(pcm.tobytes())


def export_run(conn, run_id, out_dir, pad_ms=DEFAULT_PAD_MS,
               manifest_name="manifest.json"):
    run = load_run(conn, run_id)
    if run is None:
        raise KeyError(f"run {run_id} not found")
    samples, rate = wavio.read_wav(run["source_path"])
    pad = int(round(rate * pad_ms / 1000.0))
    out_path = Path(out_dir)
    out_path.mkdir(parents=True, exist_ok=True)
    stem = Path(run["source_path"]).stem
    files = []
    for seg in run["segments"]:
        chunk = samples[seg["start_sample"]:seg["end_sample"]]
        if pad:
            chunk = np.concatenate([np.zeros(pad, np.float32), chunk,
                                    np.zeros(pad, np.float32)])
        name = f"{stem}_seg{seg['ordinal']:03d}.wav"
        target = out_path / name
        _write_wav16(target, chunk, rate)
        files.append({
            "file": name,
            "ordinal": seg["ordinal"],
            "start_sample": seg["start_sample"],
            "end_sample": seg["end_sample"],
            "pad_samples": pad,
            "duration_samples": int(chunk.shape[0]),
            "sha256": wavio.file_sha256(target),
        })
    manifest = {
        "run_id": run_id,
        "source_path": run["source_path"],
        "sample_rate": rate,
        "pad_ms": pad_ms,
        "pad_samples": pad,
        "file_count": len(files),
        "files": files,
    }
    (out_path / manifest_name).write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return manifest
