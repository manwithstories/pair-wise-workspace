"""Versioned calibration profiles with integrity checksum."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

from .errors import ProfileError

PROFILE_VERSION = 1
_FIELDS = ("sample_rate", "frame_ms", "hop_ms", "noise_floor_db",
           "high_offset_db", "low_offset_db", "fade_offset_db",
           "fade_limit_ms", "min_silence_ms", "min_segment_ms")


def _checksum(payload: dict) -> str:
    body = {k: payload[k] for k in _FIELDS}
    body["version"] = payload["version"]
    blob = json.dumps(body, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(blob).hexdigest()


def create_profile(sample_rate: int, frame_ms: float, hop_ms: float,
                   noise_floor_db: float, **overrides) -> dict:
    profile = {
        "version": PROFILE_VERSION,
        "sample_rate": int(sample_rate),
        "frame_ms": float(frame_ms),
        "hop_ms": float(hop_ms),
        "noise_floor_db": float(noise_floor_db),
        "high_offset_db": 9.0,
        "low_offset_db": 5.0,
        "fade_offset_db": 6.0,
        "fade_limit_ms": 150.0,
        "min_silence_ms": 180.0,
        "min_segment_ms": 120.0,
    }
    profile.update(overrides)
    profile["checksum"] = _checksum(profile)
    return profile


def save_profile(profile: dict, path) -> None:
    Path(path).write_text(json.dumps(profile, indent=2, sort_keys=True))


def load_profile(path) -> dict:
    try:
        profile = json.loads(Path(path).read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise ProfileError(f"cannot read profile {path}: {exc}") from exc
    if profile.get("version") != PROFILE_VERSION:
        raise ProfileError(f"profile version mismatch in {path}")
    missing = [k for k in _FIELDS if k not in profile]
    if missing:
        raise ProfileError(f"profile {path} missing fields: {missing}")
    if profile.get("checksum") != _checksum(profile):
        raise ProfileError(f"profile checksum mismatch: {path}")
    return profile
