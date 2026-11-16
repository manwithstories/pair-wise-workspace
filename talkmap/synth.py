"""Deterministic fixture synthesis with frame-aligned ground truth."""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from .util import safe_resolve
from .wavio import write_pcm_wav

_SPEECH_DB = (-26.0, -22.0)
_CROWD_DB = -63.0
_BREATH_DB = -66.0
_FLOOR_DB = (-70.0, -67.0, -71.0, -68.0)
_MASTERS = [
    {"bits": 16, "channels": 1, "jump_db": 0.0},
    {"bits": 24, "channels": 2, "jump_db": 5.0},
    {"bits": 32, "channels": 1, "jump_db": 7.0},
    {"bits": 16, "channels": 2, "jump_db": 0.0},
]


def _db_to_rms(db: float) -> float:
    return 10.0 ** (db / 20.0)


def _envelope(n: int, fade_in: int, fade_out: int, rng) -> np.ndarray:
    env = np.ones(n, dtype=np.float64)
    fade_in = min(fade_in, n // 2)
    fade_out = min(fade_out, n // 2)
    if fade_in + fade_out > n:
        fade_out = max(0, n - fade_in)
    if fade_in:
        if fade_in >= 40 * (48000 // 1000):
            progress = np.linspace(0.0, 1.0, fade_in) ** 0.5
            ramp = 0.03 * (1.0 / 0.03) ** progress
        else:
            ramp = np.linspace(0.0, 1.0, fade_in)
        env[:fade_in] = ramp
    if fade_out:
        if fade_out >= 40 * (48000 // 1000):
            progress = np.linspace(1.0, 0.0, fade_out) ** 0.5
            env[-fade_out:] = 0.03 * (1.0 / 0.03) ** progress
        else:
            env[-fade_out:] = np.linspace(1.0, 0.0, fade_out)
    wobble = 1.0 + 0.08 * np.sin(2.0 * np.pi * rng.random() * np.arange(n) / n)
    return env * wobble


def synth_master(seed: int, minutes: int, spec: dict) -> tuple[np.ndarray, list[dict]]:
    rate = 48000
    hop = rate // 100
    total = minutes * 60 * rate
    schedule_rng = np.random.default_rng(seed * 7 + 1)
    noise_rng = np.random.default_rng(seed * 13 + 5)
    voice_rng = np.random.default_rng(seed * 17 + 11)
    floor_db = _FLOOR_DB[seed % len(_FLOOR_DB)]
    base = _db_to_rms(floor_db) * noise_rng.standard_normal(total)
    jump_at = total
    if spec["jump_db"]:
        jump_at = (total // 2 // hop) * hop
        base[jump_at:] *= 10.0 ** (spec["jump_db"] / 20.0)
    planned: list[dict] = []
    cursor = 2 * hop
    while cursor < total - 6 * hop:
        talk_hops = int(schedule_rng.integers(450, 780))
        gap_hops = int(schedule_rng.integers(110, 260))
        start = cursor
        end = min(total - 2 * hop, start + talk_hops * hop)
        if end - start < 25 * hop:
            break
        fade_in_hops = int(schedule_rng.choice([0, 1, 55, 70]))
        fade_out_hops = int(schedule_rng.choice([0, 1, 4, 6, 55, 70]))
        length = end - start
        tone = 0.55 * np.sin(
            2 * np.pi * (140 + voice_rng.integers(0, 60)) * np.arange(length) / rate)
        tone += 0.45 * voice_rng.standard_normal(length)
        level = _db_to_rms(voice_rng.uniform(*_SPEECH_DB))
        env = _envelope(length, fade_in_hops * hop, fade_out_hops * hop, voice_rng)
        base[start:end] += tone * env * level
        if fade_in_hops and schedule_rng.random() < 0.7:
            breath_n = 3 * hop
            b_start = max(0, start - breath_n - hop)
            base[b_start:b_start + breath_n] += (
                _db_to_rms(_BREATH_DB) * noise_rng.standard_normal(breath_n))
        if schedule_rng.random() < 0.25:
            crowd_n = 4 * hop
            c_start = end + 2 * hop
            if c_start + crowd_n < total:
                swell = np.hanning(crowd_n) * _db_to_rms(_CROWD_DB)
                base[c_start:c_start + crowd_n] += (
                    swell * noise_rng.standard_normal(crowd_n))
        planned.append({
            "start": start, "end": end,
            "fade_in_ms": fade_in_hops * 10 if fade_in_hops > 40 else None,
            "fade_out_ms": fade_out_hops * 10 if fade_out_hops > 40 else None,
        })
        cursor = end + gap_hops * hop
    tail_n = 6 * hop
    base[-tail_n:] *= np.linspace(1.0, 0.0, tail_n)

    from .config import Params
    from .detect import label_frames
    from .framing import Framer, rms_to_dbfs
    from .noise import estimate_noise_db
    from .refine import refine_segments
    params = Params()
    framer = Framer(rate, params.frame_ms, params.hop_ms)
    rms_levels: list[float] = []
    for block_start in range(0, total, rate // 4):
        rms_levels.extend(
            f.rms for f in framer.push(base[block_start:block_start + rate // 4]))
    framer.finish()
    frame_db = rms_to_dbfs(np.asarray(rms_levels))
    noise_db = estimate_noise_db(frame_db, params.hop_ms, params)
    labels = label_frames(frame_db, noise_db, params)
    segments = refine_segments(frame_db, noise_db, labels, framer, total, params)
    truth: list[dict] = []
    for idx, segment in enumerate(segments):
        plan = planned[idx] if idx < len(planned) else {}
        truth.append({
            "seg_index": idx,
            "start_sample": int(segment.start_sample),
            "end_sample": int(segment.end_sample),
            "fade_in_ms": plan.get("fade_in_ms"),
            "fade_out_ms": plan.get("fade_out_ms"),
        })
    return base.astype(np.float64), truth


def _stereo_duplicate(samples: np.ndarray, rng) -> np.ndarray:
    """Interleave two coherent channels frame-by-frame (L,R,L,R,...)."""
    right = samples * (0.96 + 0.04 * rng.random())
    stereo = np.empty(samples.size * 2, dtype=samples.dtype)
    stereo[0::2] = samples
    stereo[1::2] = right
    return stereo


def synth_fixtures(out_dir: str, seed: int, masters: int, minutes: int) -> dict:
    out = safe_resolve(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    truth: dict[str, dict] = {}
    for idx in range(masters):
        spec = _MASTERS[idx % len(_MASTERS)]
        samples, seg_truth = synth_master(seed + idx * 101, minutes, spec)
        rng = np.random.default_rng(seed + idx)
        if spec["channels"] == 2:
            payload = _stereo_duplicate(samples, rng)
        else:
            payload = samples
        name = f"master_{idx + 1:02d}_{spec['bits']}bit_{spec['channels']}ch.wav"
        write_pcm_wav(out / name, payload, 48000, spec["bits"],
                        spec["channels"])
        truth[name] = {"sample_rate": 48000, "segments": seg_truth}
    truth_path = out / "truth.json"
    truth_path.write_text(json.dumps(truth, indent=2, ensure_ascii=False))
    return {"dir": str(out), "files": sorted(truth), "truth": str(truth_path)}
