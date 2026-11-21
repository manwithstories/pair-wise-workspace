"""Project-level configuration and work-directory layout.

The config is a plain dictionary so its digest is stable across runs.  Defaults
are chosen to keep the demo fast: a 2 s window, 0.4 s segments, three
renditions.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Mapping

from .encode import Rendition
from .model import Rate
from .segment import DEFAULT_SEGMENT_DURATION

DEFAULT_WINDOW_SECONDS = Fraction(101, 50)
DEFAULT_RENDITIONS = (
    Rendition("360p", 320, 180, 300_000, 64_000),
    Rendition("180p", 160, 90, 140_000, 48_000),
)
MIN_RENDITIONS = 2


class ConfigError(ValueError):
    """Invalid project configuration."""


@dataclass(frozen=True)
class ProjectConfig:
    window_start: Fraction = Fraction(0)
    window_end: Fraction = DEFAULT_WINDOW_SECONDS
    video_rate: Rate = Rate(25)
    segment_duration: Fraction = DEFAULT_SEGMENT_DURATION
    renditions: tuple[Rendition, ...] = DEFAULT_RENDITIONS
    color_policy: str = "first"
    color_track: str | None = None
    audio_gains: dict[str, float] = field(default_factory=dict)
    policy: str = "stop"
    retry_keyframes: bool = True

    @property
    def duration(self) -> Fraction:
        return self.window_end - self.window_start

    def validate(self) -> None:
        if self.window_end <= self.window_start:
            raise ConfigError("window end must be greater than start")
        if self.window_start < 0:
            raise ConfigError("window start must not be negative")
        if self.video_rate.numerator <= 0:
            raise ConfigError("video rate must be positive")
        if self.segment_duration <= 0:
            raise ConfigError("segment duration must be positive")
        if len(self.renditions) < MIN_RENDITIONS:
            raise ConfigError(f"at least {MIN_RENDITIONS} renditions are required")
        names = [r.name for r in self.renditions]
        if len(set(names)) != len(names):
            raise ConfigError("rendition names must be unique")
        for rendition in self.renditions:
            if rendition.width <= 0 or rendition.height <= 0:
                raise ConfigError(f"rendition {rendition.name!r} has an invalid size")
            if rendition.width % 2 or rendition.height % 2:
                raise ConfigError(f"rendition {rendition.name!r} must use even dimensions")
        if self.color_policy not in ("first", "track", "explicit"):
            raise ConfigError(f"unknown colour policy {self.color_policy!r}")
        if self.policy not in ("stop", "skip"):
            raise ConfigError(f"unknown failure policy {self.policy!r}")

    def digest_payload(self) -> dict[str, Any]:
        return {
            "window": [str(self.window_start), str(self.window_end)],
            "video_rate": str(self.video_rate),
            "segment_duration": str(self.segment_duration),
            "renditions": [r.to_dict() for r in self.renditions],
            "color_policy": self.color_policy,
            "color_track": self.color_track,
            "audio_gains": {k: float(v) for k, v in sorted(self.audio_gains.items())},
            "policy": self.policy,
            "retry_keyframes": self.retry_keyframes,
        }

    def to_json(self) -> str:
        return json.dumps(self.digest_payload(), sort_keys=True, indent=2)

    @classmethod
    def from_mapping(cls, raw: Mapping[str, Any]) -> "ProjectConfig":
        renditions: list[Rendition] = []
        for entry in raw.get("renditions", []) or []:
            if isinstance(entry, str):
                continue
            renditions.append(
                Rendition(
                    name=str(entry["name"]),
                    width=int(entry["width"]),
                    height=int(entry["height"]),
                    video_bitrate=int(entry.get("video_bitrate", 300_000)),
                    audio_bitrate=int(entry.get("audio_bitrate", 64_000)),
                    preset=str(entry.get("preset", "ultrafast")),
                    crf=int(entry.get("crf", 30)),
                )
            )
        window = raw.get("window", {})
        if isinstance(window, (list, tuple)):
            window_start, window_end = window[0], window[1]
        else:
            window_start = window.get("start", 0)
            window_end = window.get("end", DEFAULT_WINDOW_SECONDS)
        config = cls(
            window_start=Fraction(window_start),
            window_end=Fraction(window_end),
            video_rate=Rate.parse(str(raw.get("video_rate", "25"))),
            segment_duration=Fraction(raw.get("segment_duration", DEFAULT_SEGMENT_DURATION)),
            renditions=tuple(renditions) if renditions else DEFAULT_RENDITIONS,
            color_policy=str(raw.get("color_policy", "first")),
            color_track=raw.get("color_track"),
            audio_gains={k: float(v) for k, v in (raw.get("audio_gains") or {}).items()},
            policy=str(raw.get("policy", "stop")),
            retry_keyframes=bool(raw.get("retry_keyframes", True)),
        )
        config.validate()
        return config


def work_dirs(workdir: str) -> dict[str, str]:
    return {
        "root": workdir,
        "samples": os.path.join(workdir, "samples"),
        "probe": os.path.join(workdir, "probe"),
        "normalized": os.path.join(workdir, "normalized"),
        "segments": os.path.join(workdir, "segments"),
        "logs": os.path.join(workdir, "logs"),
        "manifest": os.path.join(workdir, "manifest.json"),
        "checkpoint": os.path.join(workdir, "checkpoint.json"),
        "summary": os.path.join(workdir, "summary.json"),
        "timeline": os.path.join(workdir, "timeline.json"),
    }


def ensure_layout(workdir: str) -> dict[str, str]:
    paths = work_dirs(workdir)
    for key in ("samples", "probe", "normalized", "segments", "logs"):
        os.makedirs(paths[key], exist_ok=True)
    return paths
