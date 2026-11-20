"""Explicit timeline and track graph.

A timeline is loaded from JSON and describes:

* a **master time base** (a rational number of seconds per tick),
* **tracks** - video raw bitstreams or PCM/WAV audio, each with an optional
  keyframe index and explicit dependencies on other tracks,
* **clips** - a source window (``source_in`` + ``duration``) placed at a
  timeline position (``start``) on one track, with optional dependencies on
  other clips.

All positions are integers in the master time base.  Durations of audio clips
are additionally resolved to whole samples using exact rational arithmetic, so
a 1/3-second clip at 48 kHz is always exactly 16000 samples and never drifts.

Validation is strict and precise:

* unknown track/clip references are rejected;
* dependency cycles are rejected with the full cycle path, e.g.
  ``a -> b -> a``;
* clips on one track may not overlap (the overlapping interval is reported);
* tracks that no clip references are *excluded* and reported, never silently
  composed.

Positions live in an integer tick domain, so no floating point rounding can
accumulate across clips.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from enum import Enum
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

from .bitstream import Codec
from .clock import MediaTime, MediaWindow, Rounding, TimeBase, round_rational
from .errors import ErrorCategory, make_error

__all__ = [
    "TrackKind",
    "Track",
    "Clip",
    "Timeline",
    "AudioLayout",
    "load_timeline",
    "topological_order",
    "find_cycle",
]

class TrackKind(str, Enum):
    VIDEO = "video"
    AUDIO = "audio"

    @classmethod
    def parse(cls, text: str) -> "TrackKind":
        try:
            return cls(text.lower())
        except ValueError:
            raise make_error(ErrorCategory.TIMELINE_INVALID, f"unknown track kind {text!r}") from None


@dataclass(frozen=True)
class AudioLayout:
    """Exact sample-domain description of an audio track."""

    sample_rate: int
    channels: int

    def __post_init__(self) -> None:
        if self.sample_rate <= 0:
            raise make_error(ErrorCategory.AUDIO_FORMAT, "sample rate must be positive", sample_rate=self.sample_rate)
        if not 1 <= self.channels <= 8:
            raise make_error(ErrorCategory.AUDIO_FORMAT, "channel count out of range", channels=self.channels)


@dataclass
class Track:
    id: str
    kind: TrackKind
    path: Path
    codec: Codec | None = None
    index_path: Path | None = None
    audio: AudioLayout | None = None
    depends_on: tuple[str, ...] = ()
    # exact offset applied to this track, in master ticks (may be negative)
    offset_ticks: int = 0
    # video tracks may declare their own frame rate; default is the timeline's
    frame_rate: Fraction | None = None

    def to_json(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "id": self.id,
            "kind": self.kind.value,
            "path": self.path.name,
            "depends_on": list(self.depends_on),
        }
        if self.codec is not None:
            payload["codec"] = self.codec.value
        if self.audio is not None:
            payload["sample_rate"] = self.audio.sample_rate
            payload["channels"] = self.audio.channels
        if self.offset_ticks:
            payload["offset_ticks"] = self.offset_ticks
        if self.frame_rate is not None:
            payload["frame_rate"] = f"{self.frame_rate.numerator}/{self.frame_rate.denominator}"
        return payload


@dataclass
class Clip:
    id: str
    track: str
    start_ticks: int
    source_in_ticks: int
    duration_ticks: int
    depends_on: tuple[str, ...] = ()

    @property
    def end_ticks(self) -> int:
        return self.start_ticks + self.duration_ticks

    def to_json(self) -> dict[str, Any]:
        payload = {
            "id": self.id,
            "track": self.track,
            "start_ticks": self.start_ticks,
            "source_in_ticks": self.source_in_ticks,
            "duration_ticks": self.duration_ticks,
        }
        if self.depends_on:
            payload["depends_on"] = list(self.depends_on)
        return payload


@dataclass
class Timeline:
    time_base: TimeBase
    tracks: dict[str, Track]
    clips: list[Clip]
    width: int = 160
    height: int = 90
    frame_rate: Fraction = Fraction(25, 1)
    output_channels: int = 2
    output_sample_rate: int = 48000
    notes: list[str] = field(default_factory=list)
    excluded_tracks: list[str] = field(default_factory=list)

    # ---------------------------------------------------------------- queries
    def window(self, clip: Clip) -> MediaWindow:
        return MediaWindow(
            MediaTime(clip.start_ticks, self.time_base),
            MediaTime(clip.duration_ticks, self.time_base),
        )

    def duration_ticks(self) -> int:
        if not self.clips:
            return 0
        return max(clip.end_ticks for clip in self.clips)

    def duration(self) -> MediaTime:
        return MediaTime(self.duration_ticks(), self.time_base)

    def track_frame_rate(self, track_id: str) -> Fraction:
        track = self.tracks[track_id]
        return track.frame_rate if track.frame_rate is not None else self.frame_rate

    def clips_for(self, track_id: str) -> list[Clip]:
        return sorted((c for c in self.clips if c.track == track_id), key=lambda c: c.start_ticks)

    def referenced_tracks(self) -> set[str]:
        return {clip.track for clip in self.clips}

    def active_tracks(self) -> dict[str, Track]:
        referenced = self.referenced_tracks()
        return {tid: track for tid, track in self.tracks.items() if tid in referenced}

    def ticks_to_seconds(self, ticks: int) -> Fraction:
        return Fraction(ticks) * self.time_base.seconds_per_tick

    def ticks_to_samples(self, ticks: int, rate: int, mode: Rounding = Rounding.FLOOR) -> int:
        """Exact conversion of master ticks to audio sample index."""
        seconds = Fraction(ticks) * self.time_base.seconds_per_tick
        return round_rational(seconds * rate, mode)

    def samples_to_ticks(self, samples: int, rate: int, mode: Rounding = Rounding.HALF_EVEN) -> int:
        seconds = Fraction(samples, rate)
        return round_rational(seconds / self.time_base.seconds_per_tick, mode)

    def to_json(self) -> dict[str, Any]:
        return {
            "time_base": str(self.time_base),
            "width": self.width,
            "height": self.height,
            "frame_rate": f"{self.frame_rate.numerator}/{self.frame_rate.denominator}",
            "output_channels": self.output_channels,
            "output_sample_rate": self.output_sample_rate,
            "tracks": [t.to_json() for t in self.tracks.values()],
            "clips": [c.to_json() for c in self.clips],
            "duration_seconds": float(self.duration().seconds()),
            "active_tracks": sorted(self.active_tracks()),
            "excluded_tracks": sorted(self.excluded_tracks),
            "notes": list(self.notes),
        }

    def digest(self) -> str:
        """Stable digest of the semantic timeline (paths + geometry)."""
        import hashlib

        h = hashlib.sha256()
        h.update(
            f"tb={self.time_base};{self.width}x{self.height};{self.frame_rate};"
            f"{self.output_channels}ch@{self.output_sample_rate};".encode()
        )
        for tid in sorted(self.tracks):
            track = self.tracks[tid]
            h.update(
                f"T:{tid}:{track.kind.value}:{track.path.name}:{track.offset_ticks}:"
                f"{track.frame_rate or self.frame_rate};".encode()
            )
        for clip in sorted(self.clips, key=lambda c: (c.start_ticks, c.id)):
            h.update(
                f"C:{clip.id}:{clip.track}:{clip.start_ticks}:{clip.source_in_ticks}:{clip.duration_ticks};".encode()
            )
        return h.hexdigest()


# ==========================================================================
# Loading
# ==========================================================================


def _require_int(payload: dict[str, Any], key: str, *, where: str) -> int:
    value = payload.get(key, 0)
    if isinstance(value, bool) or not isinstance(value, int):
        raise make_error(
            ErrorCategory.TIMELINE_INVALID,
            f"{where}.{key} must be an integer tick count",
            value=value,
        )
    return value


def _parse_rate(text: Any, where: str) -> Fraction:
    if text is None:
        return Fraction(25, 1)
    if isinstance(text, int) and not isinstance(text, bool):
        return Fraction(text, 1)
    try:
        if isinstance(text, str):
            rate = Fraction(text)
            if rate <= 0:
                raise ValueError("frame rate must be positive")
            return rate
    except (ValueError, ZeroDivisionError) as exc:
        raise make_error(ErrorCategory.TIMELINE_INVALID, f"{where}: invalid frame rate", value=text) from exc
    raise make_error(ErrorCategory.TIMELINE_INVALID, f"{where}: invalid frame rate", value=text)


def _resolve_path(root: Path, text: str, where: str) -> Path:
    candidate = Path(text)
    resolved = (candidate if candidate.is_absolute() else root / candidate).resolve()
    root_resolved = root.resolve()
    try:
        resolved.relative_to(root_resolved)
    except ValueError:
        raise make_error(
            ErrorCategory.PATH_ESCAPE,
            f"{where} escapes the timeline directory",
            path=str(resolved),
            root=str(root_resolved),
        ) from None
    return resolved


def _parse_track(raw: Any, root: Path, pos: int) -> Track:
    where = f"tracks[{pos}]"
    if not isinstance(raw, dict) or not isinstance(raw.get("id"), str) or not raw["id"]:
        raise make_error(ErrorCategory.TIMELINE_INVALID, f"{where} must have a non-empty id")
    track_id = raw["id"]
    kind = TrackKind.parse(str(raw.get("kind", "")))
    if "path" not in raw:
        raise make_error(ErrorCategory.TIMELINE_INVALID, f"{where}.path is required", track=track_id)
    path = _resolve_path(root, str(raw["path"]), f"{where}.path")
    codec = Codec.parse(raw["codec"]) if raw.get("codec") else None
    audio = None
    if kind is TrackKind.AUDIO:
        if "sample_rate" not in raw:
            raise make_error(ErrorCategory.AUDIO_FORMAT, f"{where}.sample_rate is required", track=track_id)
        audio = AudioLayout(int(raw["sample_rate"]), int(raw.get("channels", 1)))
    elif codec is None:
        raise make_error(ErrorCategory.TIMELINE_INVALID, f"{where}.codec is required for video", track=track_id)
    index_path = None
    if raw.get("index"):
        index_path = _resolve_path(root, str(raw["index"]), f"{where}.index")
    depends = raw.get("depends_on", [])
    if not isinstance(depends, list) or not all(isinstance(d, str) for d in depends):
        raise make_error(ErrorCategory.TIMELINE_INVALID, f"{where}.depends_on must be a list of track ids")
    return Track(
        id=track_id,
        kind=kind,
        path=path,
        codec=codec,
        index_path=index_path,
        audio=audio,
        depends_on=tuple(depends),
        offset_ticks=_require_int(raw, "offset_ticks", where=where),
        frame_rate=_parse_rate(raw["frame_rate"], where) if raw.get("frame_rate") is not None else None,
    )


def _parse_clip(raw: Any, pos: int) -> Clip:
    where = f"clips[{pos}]"
    if not isinstance(raw, dict) or not isinstance(raw.get("id"), str):
        raise make_error(ErrorCategory.TIMELINE_INVALID, f"{where} must have a string id")
    if not isinstance(raw.get("track"), str):
        raise make_error(ErrorCategory.TIMELINE_INVALID, f"{where}.track is required", clip=raw["id"])
    duration = _require_int(raw, "duration_ticks", where=where)
    if duration <= 0:
        raise make_error(ErrorCategory.TIMELINE_INVALID, f"{where}.duration_ticks must be positive", clip=raw["id"])
    depends = raw.get("depends_on", [])
    if not isinstance(depends, list) or not all(isinstance(d, str) for d in depends):
        raise make_error(ErrorCategory.TIMELINE_INVALID, f"{where}.depends_on must be a list of clip ids")
    return Clip(
        id=raw["id"],
        track=raw["track"],
        start_ticks=_require_int(raw, "start_ticks", where=where),
        source_in_ticks=_require_int(raw, "source_in_ticks", where=where),
        duration_ticks=duration,
        depends_on=tuple(depends),
    )


def load_timeline(path: Path, *, base_dir: Path | None = None, strict: bool = False) -> Timeline:
    """Load and validate a timeline JSON document."""
    path = Path(path)
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise make_error(ErrorCategory.TIMELINE_INVALID, "timeline file not found", path=str(path)) from None
    except json.JSONDecodeError as exc:
        raise make_error(
            ErrorCategory.TIMELINE_INVALID, f"timeline is not valid JSON: {exc.msg}", line=exc.lineno
        ) from exc
    if not isinstance(payload, dict):
        raise make_error(ErrorCategory.TIMELINE_INVALID, "timeline root must be an object")

    root = Path(base_dir) if base_dir is not None else path.parent
    try:
        time_base = TimeBase.parse(str(payload.get("time_base", "1/90000")))
    except (ValueError, TypeError) as exc:
        raise make_error(ErrorCategory.TIMELINE_INVALID, f"invalid time_base: {exc}") from exc

    raw_tracks = payload.get("tracks", [])
    if not isinstance(raw_tracks, list) or not raw_tracks:
        raise make_error(ErrorCategory.TIMELINE_INVALID, "timeline must declare at least one track")
    tracks: dict[str, Track] = {}
    for pos, raw in enumerate(raw_tracks):
        track = _parse_track(raw, root, pos)
        if track.id in tracks:
            raise make_error(ErrorCategory.TIMELINE_INVALID, f"duplicate track id {track.id!r}", track=track.id)
        tracks[track.id] = track

    raw_clips = payload.get("clips", [])
    if not isinstance(raw_clips, list):
        raise make_error(ErrorCategory.TIMELINE_INVALID, "clips must be a list")
    clips = [_parse_clip(raw, pos) for pos, raw in enumerate(raw_clips)]
    if len({c.id for c in clips}) != len(clips):
        raise make_error(ErrorCategory.TIMELINE_INVALID, "duplicate clip id")

    timeline = Timeline(
        time_base=time_base,
        tracks=tracks,
        clips=clips,
        width=int(payload.get("width", 160)),
        height=int(payload.get("height", 90)),
        frame_rate=_parse_rate(payload.get("frame_rate"), "timeline"),
        output_channels=int(payload.get("output_channels", 2)),
        output_sample_rate=int(payload.get("output_sample_rate", 48000)),
    )
    validate_timeline(timeline, strict=strict)
    return timeline
# Graph validation
