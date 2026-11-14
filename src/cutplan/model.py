from __future__ import annotations

from copy import deepcopy
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any

from .errors import CutplanError
from .timebase import FrameInterval, FrameRate, parse_rate


def _check_int(name: str, value: object, *, minimum: int | None = None) -> None:
    if not isinstance(value, int) or isinstance(value, bool):
        raise CutplanError("E_MODEL_TYPE", f"{name} must be an integer")
    if minimum is not None and value < minimum:
        raise CutplanError("E_MODEL_RANGE", f"{name} must be >= {minimum}")


def _check_dict(name: str, value: object) -> None:
    if not isinstance(value, dict):
        raise CutplanError("E_MODEL_TYPE", f"{name} must be an object")


@dataclass(slots=True)
class Clip:
    id: str
    start: int
    duration: int
    source_start: int = 0
    priority: int = 0
    role: str = ""
    ext: dict[str, Any] = field(default_factory=dict)
    media_ref: str = ""

    def __post_init__(self) -> None:
        if not isinstance(self.id, str) or not self.id:
            raise CutplanError("E_MODEL_ID", "clip id must be a non-empty string")
        _check_int("clip.start", self.start, minimum=0)
        _check_int("clip.duration", self.duration, minimum=0)
        _check_int("clip.source_start", self.source_start, minimum=0)
        _check_int("clip.priority", self.priority)
        if not isinstance(self.role, str) or not isinstance(self.media_ref, str):
            raise CutplanError("E_MODEL_TYPE", "clip role and media_ref must be strings")
        _check_dict("clip.ext", self.ext)

    @property
    def end(self) -> int:
        return self.start + self.duration

    @property
    def interval(self) -> FrameInterval:
        return FrameInterval(self.start, self.end)

    @property
    def zero_length(self) -> bool:
        return self.duration == 0

    def order_key(self) -> tuple[int, str]:
        return (-self.priority, self.id)

    def clone(self) -> "Clip":
        return deepcopy(self)


@dataclass(slots=True)
class Track:
    id: str
    kind: str
    clips: list[Clip] = field(default_factory=list)
    priority: int = 0
    ext: dict[str, Any] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if not isinstance(self.id, str) or not self.id:
            raise CutplanError("E_MODEL_ID", "track id must be a non-empty string")
        if not isinstance(self.kind, str) or not self.kind:
            raise CutplanError("E_MODEL_ID", "track kind must be a non-empty string")
        _check_int("track.priority", self.priority)
        if not isinstance(self.clips, list) or any(not isinstance(c, Clip) for c in self.clips):
            raise CutplanError("E_MODEL_TYPE", "track.clips must contain Clip values")
        _check_dict("track.ext", self.ext)
        _check_dict("track.metadata", self.metadata)

    @property
    def sorted_clips(self) -> list[Clip]:
        return sorted(self.clips, key=lambda c: (c.start, c.end, c.id))

    @property
    def intervals(self) -> list[FrameInterval]:
        return [clip.interval for clip in self.sorted_clips]

    def clip(self, clip_id: str) -> Clip:
        for clip in self.clips:
            if clip.id == clip_id:
                return clip
        raise CutplanError("E_CLIP_NOT_FOUND", f"clip not found: {clip_id}")

    def clone(self) -> "Track":
        return deepcopy(self)


@dataclass(slots=True)
class Manifest:
    title: str = ""
    schema: str = "cutplan/v2"
    ext: dict[str, Any] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if not isinstance(self.title, str) or not isinstance(self.schema, str):
            raise CutplanError("E_MODEL_TYPE", "manifest title and schema must be strings")
        _check_dict("manifest.ext", self.ext)
        _check_dict("manifest.metadata", self.metadata)

    def clone(self) -> "Manifest":
        return deepcopy(self)


@dataclass(slots=True)
class Timeline:
    rate: FrameRate
    declared_duration: int
    tracks: list[Track] = field(default_factory=list)
    manifest: Manifest = field(default_factory=Manifest)
    ext: dict[str, Any] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)
    source_format: str = ""

    def __post_init__(self) -> None:
        if not isinstance(self.rate, FrameRate):
            self.rate = parse_rate(self.rate)
        _check_int("timeline.declared_duration", self.declared_duration, minimum=0)
        if not isinstance(self.tracks, list) or any(not isinstance(t, Track) for t in self.tracks):
            raise CutplanError("E_MODEL_TYPE", "timeline.tracks must contain Track values")
        if not isinstance(self.manifest, Manifest):
            raise CutplanError("E_MODEL_TYPE", "timeline.manifest must be a Manifest")
        _check_dict("timeline.ext", self.ext)
        _check_dict("timeline.metadata", self.metadata)
        if not isinstance(self.source_format, str):
            raise CutplanError("E_MODEL_TYPE", "source_format must be a string")
        self.validate_invariants()

    @property
    def content_end(self) -> int:
        return max((clip.end for track in self.tracks for clip in track.clips), default=0)

    @property
    def duration_frames(self) -> int:
        return self.content_end

    @property
    def output_duration_frames(self) -> int:
        return max(self.declared_duration, self.content_end)

    @property
    def duration_seconds(self) -> Fraction:
        return Fraction(self.duration_frames, 1) / self.rate.fps

    @property
    def declared_seconds(self) -> Fraction:
        return Fraction(self.declared_duration, 1) / self.rate.fps

    @property
    def clip_count(self) -> int:
        return sum(len(track.clips) for track in self.tracks)

    def track(self, track_id: str) -> Track:
        for track in self.tracks:
            if track.id == track_id:
                return track
        raise CutplanError("E_TRACK_NOT_FOUND", f"track not found: {track_id}")

    def validate_invariants(self) -> None:
        track_ids: set[str] = set()
        clip_ids: set[str] = set()
        for track in self.tracks:
            if track.id in track_ids:
                raise CutplanError("E_DUPLICATE_TRACK_ID", f"duplicate track id: {track.id}")
            track_ids.add(track.id)
            local_ids: set[str] = set()
            for clip in track.clips:
                if clip.id in local_ids:
                    raise CutplanError("E_DUPLICATE_CLIP_ID", f"duplicate clip id in track: {clip.id}")
                if clip.id in clip_ids:
                    raise CutplanError("E_DUPLICATE_CLIP_ID", f"duplicate clip id: {clip.id}")
                local_ids.add(clip.id)
                clip_ids.add(clip.id)

    def clone(self) -> "Timeline":
        return deepcopy(self)

    def canonical_dict(self) -> dict[str, Any]:
        return {
            "rate": str(self.rate),
            "declared_duration": self.declared_duration,
            "manifest": {
                "title": self.manifest.title,
                "schema": self.manifest.schema,
                "ext": _plain(self.manifest.ext),
                "metadata": _plain(self.manifest.metadata),
            },
            "ext": _plain(self.ext),
            "metadata": _plain(self.metadata),
            "tracks": [
                {
                    "id": track.id,
                    "kind": track.kind,
                    "priority": track.priority,
                    "ext": _plain(track.ext),
                    "metadata": _plain(track.metadata),
                    "clips": [
                        {
                            "id": clip.id,
                            "start": clip.start,
                            "duration": clip.duration,
                            "source_start": clip.source_start,
                            "priority": clip.priority,
                            "role": clip.role,
                            "media_ref": clip.media_ref,
                            "ext": _plain(clip.ext),
                        }
                        for clip in sorted(track.clips, key=lambda c: (c.start, c.id))
                    ],
                }
                for track in sorted(self.tracks, key=lambda t: t.id)
            ],
        }


def _plain(value: Any) -> Any:
    if isinstance(value, dict):
        return {str(key): _plain(item) for key, item in sorted(value.items(), key=lambda kv: str(kv[0]))}
    if isinstance(value, (list, tuple)):
        return [_plain(item) for item in value]
    return value
