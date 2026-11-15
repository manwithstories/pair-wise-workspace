"""Exact, immutable timeline kernel shared by every pipeline stage."""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from fractions import Fraction
from types import MappingProxyType
from typing import Any, Iterable, Mapping


class ModelError(ValueError):
    """Raised when a timeline value cannot be represented exactly."""

    def __init__(self, code: str, message: str, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details


def parse_rational(value: Any, *, field_name: str = "value") -> Fraction:
    """Parse ints, num/den strings, or {num, den} without using floats."""
    if isinstance(value, Fraction):
        return value
    if isinstance(value, bool):
        raise ModelError("invalid_rational", f"{field_name} is boolean")
    if isinstance(value, int):
        return Fraction(value)
    if isinstance(value, str):
        text = value.strip()
        if "/" in text:
            num, den = text.split("/", 1)
            try:
                numerator, denominator = int(num), int(den)
            except ValueError as exc:
                raise ModelError("invalid_rational", f"bad rational {value!r}") from exc
            if denominator == 0:
                raise ModelError("zero_denominator", f"{field_name} denominator is zero")
            return Fraction(numerator, denominator)
        try:
            return Fraction(int(text))
        except ValueError as exc:
            raise ModelError("invalid_rational", f"bad rational {value!r}") from exc
    if isinstance(value, Mapping):
        try:
            num, den = value["num"], value["den"]
            if int(den) == 0:
                raise ModelError("zero_denominator", f"{field_name} denominator is zero")
            return Fraction(int(num), int(den))
        except ModelError:
            raise
        except (KeyError, TypeError, ValueError) as exc:
            raise ModelError("invalid_rational", f"bad rational object {value!r}") from exc
    raise ModelError("invalid_rational", f"unsupported rational {value!r}")


def rational_json(value: Fraction) -> dict[str, int]:
    return {"num": value.numerator, "den": value.denominator}


def _freeze_value(value: Any) -> Any:
    if isinstance(value, Mapping):
        return MappingProxyType({str(key): _freeze_value(item) for key, item in value.items()})
    if isinstance(value, (list, tuple)):
        return tuple(_freeze_value(item) for item in value)
    if isinstance(value, set):
        return frozenset(_freeze_value(item) for item in value)
    return value


def rate_json(value: Fraction) -> dict[str, int]:
    return rational_json(value)


def fraction_from_json(value: Mapping[str, Any]) -> Fraction:
    return parse_rational(value)


@dataclass(frozen=True)
class RationalFrame:
    """A frame coordinate at an exact rational rate."""

    frames: Fraction
    rate: Fraction

    def __post_init__(self) -> None:
        object.__setattr__(self, "frames", parse_rational(self.frames, field_name="frames"))
        object.__setattr__(self, "rate", parse_rational(self.rate, field_name="rate"))
        if self.rate <= 0:
            raise ModelError("invalid_rate", "frame rate must be positive")

    @classmethod
    def from_frames(cls, value: Any, rate: Any) -> "RationalFrame":
        return cls(parse_rational(value), parse_rational(rate, field_name="rate"))

    @classmethod
    def from_seconds(cls, value: Any, rate: Any) -> "RationalFrame":
        seconds = parse_rational(value, field_name="seconds")
        parsed_rate = parse_rational(rate, field_name="rate")
        return cls(seconds * parsed_rate, parsed_rate)

    @property
    def seconds(self) -> Fraction:
        return self.frames / self.rate

    def to_rate(self, rate: Any) -> "RationalFrame":
        target = parse_rational(rate, field_name="rate")
        return RationalFrame(self.seconds * target, target)

    def to_json(self, include_rate: bool = False) -> dict[str, Any]:
        payload: dict[str, Any] = rational_json(self.frames)
        if include_rate:
            payload["rate"] = rate_json(self.rate)
        return payload


@dataclass(frozen=True)
class Clip:
    id: str
    track: str
    src: str
    start: Fraction
    duration: Fraction
    layer: int = 0
    priority: int = 0
    locked: bool = False
    kind: str = "clip"
    policy: str | None = None
    action: str = "unchanged"
    source_start: Fraction | None = None
    source_rate: Fraction | None = None
    metadata: tuple[tuple[str, Any], ...] = ()

    def __post_init__(self) -> None:
        object.__setattr__(self, "start", parse_rational(self.start, field_name="start"))
        object.__setattr__(self, "duration", parse_rational(self.duration, field_name="duration"))
        object.__setattr__(self, "metadata", tuple(sorted(
            ((str(key), _freeze_value(value)) for key, value in self.metadata), key=lambda item: item[0])))
        if self.duration < 0:
            raise ModelError("negative_duration", f"clip {self.id} has negative duration")
        if self.kind not in {"clip", "gap"}:
            raise ModelError("invalid_clip_kind", f"clip {self.id} has kind {self.kind!r}")

    @property
    def end(self) -> Fraction:
        return self.start + self.duration

    @property
    def is_content(self) -> bool:
        return self.kind == "clip" and self.duration > 0

    def overlaps(self, other: "Clip") -> bool:
        return self.start < other.end and other.start < self.end

    def changed(self, **updates: Any) -> "Clip":
        return replace(self, **updates)

    def metadata_dict(self) -> dict[str, Any]:
        return dict(self.metadata)

    def ownership_key(self, track_order: int = 0) -> tuple[Any, ...]:
        return (self.layer, self.priority, int(self.locked), -track_order, self.id)


@dataclass(frozen=True)
class Track:
    name: str
    layer: int
    priority: int = 0
    clips: tuple[Clip, ...] = ()
    metadata: tuple[tuple[str, Any], ...] = ()

    def __post_init__(self) -> None:
        object.__setattr__(self, "clips", tuple(sorted(self.clips, key=lambda clip: (clip.start, clip.id))))
        object.__setattr__(self, "metadata", tuple(sorted(
            ((str(key), _freeze_value(value)) for key, value in self.metadata), key=lambda item: item[0])))

    def with_clips(self, clips: Iterable[Clip]) -> "Track":
        ordered = tuple(sorted(clips, key=lambda clip: (clip.start, clip.id)))
        return replace(self, clips=ordered)


@dataclass(frozen=True)
class Timeline:
    name: str
    rate: Fraction
    master_frames: int
    tracks: tuple[Track, ...] = ()
    policy: str = "protect-locked"
    source_format: int = 0
    precision_losses: tuple[dict[str, Any], ...] = ()
    metadata: tuple[tuple[str, Any], ...] = ()

    def __post_init__(self) -> None:
        object.__setattr__(self, "rate", parse_rational(self.rate, field_name="rate"))
        object.__setattr__(self, "tracks", tuple(self.tracks))
        object.__setattr__(self, "precision_losses", tuple(_freeze_value(item) for item in self.precision_losses))
        object.__setattr__(self, "metadata", tuple(sorted(
            ((str(key), _freeze_value(value)) for key, value in self.metadata), key=lambda item: item[0])))
        if self.rate <= 0:
            raise ModelError("invalid_rate", "timeline rate must be positive")
        if not isinstance(self.master_frames, int) or self.master_frames < 0:
            raise ModelError("invalid_master_frames", "master_frames must be a non-negative int")

    def with_tracks(self, tracks: Iterable[Track]) -> "Timeline":
        return replace(self, tracks=tuple(tracks))

    def snapshot(self) -> "Timeline":
        """The model is frozen; snapshot is explicit for callers requiring a copy."""
        return replace(self, tracks=tuple(self.tracks), precision_losses=tuple(self.precision_losses))

    def all_clips(self) -> tuple[Clip, ...]:
        return tuple(clip for track in self.tracks for clip in track.clips)

    def content_clips(self) -> tuple[Clip, ...]:
        return tuple(clip for clip in self.all_clips() if clip.is_content)

    def track_names(self) -> tuple[str, ...]:
        return tuple(track.name for track in self.tracks)

    def find_clip(self, clip_id: str) -> Clip | None:
        for clip in self.all_clips():
            if clip.id == clip_id:
                return clip
        return None


def make_track(name: str, clips: Iterable[Clip], *, layer: int | None = None,
               priority: int | None = None, metadata: Mapping[str, Any] | None = None) -> Track:
    clip_list = list(clips)
    inferred_layer = layer if layer is not None else (clip_list[0].layer if clip_list else 0)
    inferred_priority = priority if priority is not None else (clip_list[0].priority if clip_list else 0)
    return Track(
        name=name,
        layer=inferred_layer,
        priority=inferred_priority,
        clips=tuple(sorted(clip_list, key=lambda clip: (clip.start, clip.id))),
        metadata=tuple(sorted((metadata or {}).items())),
    )


def timeline_sort_key(track: Track) -> tuple[Any, ...]:
    return (-track.layer, -track.priority, track.name)


def clip_to_interval_dict(clip: Clip) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "id": clip.id,
        "track": clip.track,
        "start": rational_json(clip.start),
        "duration": rational_json(clip.duration),
        "src": clip.src,
        "layer": clip.layer,
        "priority": clip.priority,
        "locked": clip.locked,
        "action": clip.action,
        "kind": clip.kind,
    }
    if clip.source_start is not None:
        payload["source_start"] = rational_json(clip.source_start)
    if clip.source_rate is not None:
        payload["source_rate"] = rational_json(clip.source_rate)
    return payload


def timeline_summary(timeline: Timeline) -> dict[str, Any]:
    clips = timeline.all_clips()
    content = [clip for clip in clips if clip.is_content]
    return {
        "name": timeline.name,
        "rate": rational_json(timeline.rate),
        "master_frames": timeline.master_frames,
        "track_count": len(timeline.tracks),
        "clip_count": len(clips),
        "content_clip_count": len(content),
        "source_format": timeline.source_format,
        "policy": timeline.policy,
        "tracks": [
            {"name": track.name, "layer": track.layer, "priority": track.priority,
             "clip_count": len(track.clips)}
            for track in timeline.tracks
        ],
    }
