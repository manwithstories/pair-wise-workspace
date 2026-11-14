"""Two-pass chunked file analysis and resumable directory scanning."""

from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass, field
from pathlib import Path

from . import framing, noise, wavio
from .detector import DetectorParams, detect
from .errors import ProfileError


def sha256_file(path, block=1 << 20) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as fh:
        while chunk := fh.read(block):
            digest.update(chunk)
    return digest.hexdigest()


def analyze_file(path, profile: dict):
    """Pass 1: envelope -> noise floor. Pass 2: re-scan -> detection."""
    info = wavio.read_info(path)
    if info.sample_rate != profile["sample_rate"]:
        raise ProfileError(
            f"sample rate {info.sample_rate} != profile {profile['sample_rate']}",
            path=str(path))
    frame, hop = framing.frame_params(
        info.sample_rate, profile["frame_ms"], profile["hop_ms"])
    env1 = framing.envelope_dbfs(wavio.iter_blocks(path, info), frame, hop)
    floor = noise.estimate_noise_floor(env1.env_db)
    env2 = framing.envelope_dbfs(wavio.iter_blocks(path, info), frame, hop)
    params = DetectorParams(
        sample_rate=info.sample_rate, frame=frame, hop=hop,
        high_offset_db=profile["high_offset_db"],
        low_offset_db=profile["low_offset_db"],
        fade_offset_db=profile["fade_offset_db"],
        fade_limit_ms=profile["fade_limit_ms"],
        min_silence_ms=profile["min_silence_ms"],
        min_segment_ms=profile["min_segment_ms"])
    segments, silences = detect(env2.env_db, floor, params, env2.total_samples,
                                env2.tail_db, env2.tail_samples)
    meta = {"sample_rate": info.sample_rate, "channels": info.channels,
            "total_samples": info.num_samples}
    return meta, segments, silences


def collect_wavs(root):
    """Sorted relative paths of .wav files; symlinks are not followed."""
    root = Path(root)
    found = []
    for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
        dirnames[:] = sorted(d for d in dirnames
                             if not (Path(dirpath) / d).is_symlink())
        for name in sorted(filenames):
            p = Path(dirpath) / name
            if name.lower().endswith(".wav") and not p.is_symlink():
                found.append(p)
    return sorted(found, key=lambda p: str(p.relative_to(root)))


@dataclass
class ScanSummary:
    ok: int = 0
    failed: int = 0
    skipped: int = 0
    profile_mismatch: bool = False
    failures: list = field(default_factory=list)


def scan_directory(root, store, profile) -> ScanSummary:
    summary = ScanSummary()
    run_id = store.begin_run()
    try:
        for path in collect_wavs(root):
            rel = str(path)
            try:
                digest = sha256_file(path)
                if store.known_digest(rel) == digest:
                    summary.skipped += 1
                    continue
                meta, segments, silences = analyze_file(path, profile)
                store.record_result(rel, digest, meta, segments, silences)
                summary.ok += 1
            except Exception as exc:  # isolate per-file failure, keep going
                if isinstance(exc, ProfileError):
                    summary.profile_mismatch = True
                summary.failed += 1
                summary.failures.append((rel, str(exc)))
                try:
                    store.record_failure(rel, None, {}, exc)
                except Exception:
                    pass
    except BaseException:
        store.finish_run(run_id, summary.ok, summary.failed, "interrupted")
        raise
    status = "ok" if summary.failed == 0 else "partial"
    store.finish_run(run_id, summary.ok, summary.failed, status)
    return summary
