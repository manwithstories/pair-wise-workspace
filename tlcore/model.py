"""Core timeline model independent of OpenTimelineIO."""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterable

from .intervals import Interval
from .timebase import DEFAULT_FPS


class ModelError(ValueError):
    """Raised when timeline structure is invalid."""


@dataclass
class Segment:
    id: str
    start: int
    duration: int
    source_start: int = 0
    priority: int = 0
    locked: bool = False
    metadata: dict[str, Any] = field(default_factory=dict)
    extensions: dict[str, Any] = field(default_factory=dict)

    @property
    def end(self) -> int:
        return self.start + self.duration

    @property
    def interval(self) -> Interval:
        return Interval(self.start, self.end)

    def copy(self, **changes: Any) -> "Segment":
        values = {
            "id": self.id,
            "start": self.start,
            "duration": self.duration,
            "source_start": self.source_start,
            "priority": self.priority,
            "locked": self.locked,
            "metadata": dict(self.metadata),
            "extensions": dict(self.extensions),
        }
        values.update(changes)
        return Segment(**values)


@dataclass
class Track:
    id: str
    kind: str = "video"
    z_order: int = 0
    priority: int = 0
    locked: bool = False
    metadata: dict[str, Any] = field(default_factory=dict)
    segments: list[Segment] = field(default_factory=list)
    extensions: dict[str, Any] = field(default_factory=dict)

    def sorted_segments(self) -> list[Segment]:
        return sorted(self.segments, key=lambda segment: (segment.start, segment.id))


@dataclass
class Timeline:
    name: str = "timeline"
    fps: Fraction = DEFAULT_FPS
    tracks: list[Track] = field(default_factory=list)
    metadata: dict[str, Any] = field(default_factory=dict)
    extensions: dict[str, Any] = field(default_factory=dict)
    locked: bool = False
    declared_duration: int | None = None

    def track(self, track_id: str) -> Track:
        for track in self.tracks:
            if track.id == track_id:
                return track
        raise ModelError(f"unknown track: {track_id}")

    def iter_segments(self) -> Iterable[tuple[Track, Segment]]:
        for track in self.tracks:
            for segment in track.segments:
                yield track, segment

    def segment(self, segment_id: str) -> tuple[Track, Segment]:
        for track, segment in self.iter_segments():
            if segment.id == segment_id:
                return track, segment
        raise ModelError(f"unknown segment: {segment_id}")

    def effective_locked(self, track: Track, segment: Segment) -> bool:
        return bool(self.locked or track.locked or segment.locked)

    def computed_duration(self) -> int:
        return max((segment.end for _, segment in self.iter_segments()), default=0)

    def duration(self) -> int:
        if self.declared_duration is not None:
            return max(0, self.declared_duration)
        return self.computed_duration()

    def segment_count(self) -> int:
        return sum(len(track.segments) for track in self.tracks)

    def sorted_tracks(self) -> list[Track]:
        return sorted(self.tracks, key=lambda track: (track.z_order, track.id))

    def copy(self) -> "Timeline":
        return Timeline(
            name=self.name,
            fps=self.fps,
            tracks=[
                Track(
                    id=track.id,
                    kind=track.kind,
                    z_order=track.z_order,
                    priority=track.priority,
                    locked=track.locked,
                    metadata=dict(track.metadata),
                    segments=[segment.copy() for segment in track.segments],
                    extensions=dict(track.extensions),
                )
                for track in self.tracks
            ],
            metadata=dict(self.metadata),
            extensions=dict(self.extensions),
            locked=self.locked,
            declared_duration=self.declared_duration,
        )


def validate_model(timeline: Timeline) -> list[str]:
    errors: list[str] = []
    if timeline.fps <= 0:
        errors.append("frame rate must be positive")
    track_ids: set[str] = set()
    segment_ids: set[str] = set()
    for track in timeline.tracks:
        if not track.id:
            errors.append("track id must not be empty")
        if track.id in track_ids:
            errors.append(f"duplicate track id: {track.id}")
        track_ids.add(track.id)
        if not track.kind:
            errors.append(f"track {track.id}: kind must not be empty")
        for segment in track.segments:
            if not segment.id:
                errors.append(f"track {track.id}: segment id must not be empty")
            if segment.id in segment_ids:
                errors.append(f"duplicate segment id: {segment.id}")
            segment_ids.add(segment.id)
    return errors
