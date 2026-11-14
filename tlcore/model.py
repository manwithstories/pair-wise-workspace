"""Internal timeline model and structural invariant checks."""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from fractions import Fraction
from typing import Any, Iterable


class ModelError(ValueError):
    """The input document cannot form a structurally valid timeline."""


class InvariantError(ModelError):
    """An operation produced an internally inconsistent timeline."""


@dataclass(slots=True)
class Clip:
    id: str
    track: str
    kind: str
    start_ns: int
    duration_ns: int
    source_in_ns: int = 0
    source_duration_ns: int | None = None
    link_id: str | None = None
    priority: int = 0
    metadata: dict[str, Any] = field(default_factory=dict)

    @property
    def end_ns(self) -> int:
        return self.start_ns + self.duration_ns

    @property
    def start(self) -> int:
        return self.start_ns

    @property
    def end(self) -> int:
        return self.end_ns

    @property
    def source_out_ns(self) -> int:
        return self.source_in_ns + self.duration_ns

    @property
    def source_remaining_ns(self) -> int | None:
        if self.source_duration_ns is None:
            return None
        return max(0, self.source_duration_ns - self.source_out_ns)

    @property
    def link(self) -> str | None:
        return self.link_id


@dataclass(slots=True)
class Track:
    id: str
    kind: str
    clips: list[Clip] = field(default_factory=list)
    metadata: dict[str, Any] = field(default_factory=dict)

    @property
    def duration_ns(self) -> int:
        return max((clip.end_ns for clip in self.clips), default=0)

    def ordered_clips(self) -> list[Clip]:
        return sorted(self.clips, key=lambda clip: (clip.start_ns, clip.id))


@dataclass(slots=True)
class Transition:
    id: str
    track: str
    from_clip_id: str
    to_clip_id: str
    duration_ns: int
    max_duration_ns: int | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    @property
    def from_id(self) -> str:
        return self.from_clip_id

    @property
    def to_id(self) -> str:
        return self.to_clip_id


@dataclass(slots=True)
class LinkGroup:
    id: str
    clip_ids: list[str] = field(default_factory=list)


@dataclass(slots=True)
class Marker:
    id: str
    track: str | None
    start_ns: int
    duration_ns: int = 0
    name: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass(slots=True)
class Timeline:
    version: int = 2
    fps: Fraction = Fraction(25, 1)
    declared_duration_ns: int = 0
    tracks: list[Track] = field(default_factory=list)
    transitions: list[Transition] = field(default_factory=list)
    link_groups: list[LinkGroup] = field(default_factory=list)
    markers: list[Marker] = field(default_factory=list)
    metadata: dict[str, Any] = field(default_factory=dict)

    @property
    def computed_duration_ns(self) -> int:
        return max((track.duration_ns for track in self.tracks), default=0)

    @property
    def duration_ns(self) -> int:
        return self.computed_duration_ns

    @property
    def deviation_ns(self) -> int:
        return self.declared_duration_ns - self.computed_duration_ns

    def clip_by_id(self) -> dict[str, Clip]:
        return {clip.id: clip for track in self.tracks for clip in track.clips}

    def track_by_id(self) -> dict[str, Track]:
        return {track.id: track for track in self.tracks}

    def normalize_links(self) -> None:
        clips = self.clip_by_id()
        groups: dict[str, LinkGroup] = {}
        for group in self.link_groups:
            if group.id in groups:
                raise ModelError(f"duplicate link group id: {group.id}")
            groups[group.id] = LinkGroup(group.id, list(dict.fromkeys(group.clip_ids)))
        for group in list(groups.values()):
            for clip_id in group.clip_ids:
                clip = clips.get(clip_id)
                if clip is None:
                    raise ModelError(
                        f"link group {group.id!r} references missing clip {clip_id!r}"
                    )
                if clip.link_id and clip.link_id != group.id:
                    raise ModelError(
                        f"clip {clip.id!r} belongs to both {clip.link_id!r} and {group.id!r}"
                    )
                clip.link_id = group.id
        for clip in clips.values():
            if not clip.link_id:
                continue
            group = groups.get(clip.link_id)
            if group is None:
                groups[clip.link_id] = LinkGroup(clip.link_id, [clip.id])
            elif clip.id not in group.clip_ids:
                group.clip_ids.append(clip.id)
        self.link_groups = list(groups.values())

    def validate(self) -> None:
        if self.version not in (1, 2):
            raise ModelError(f"unsupported timeline version: {self.version}")
        if self.declared_duration_ns < 0:
            raise ModelError("declared duration must not be negative")
        if not isinstance(self.fps, Fraction) or self.fps <= 0:
            raise ModelError("fps must be a positive rational")
        track_ids: set[str] = set()
        clip_ids: set[str] = set()
        transition_ids: set[str] = set()
        for track in self.tracks:
            if not track.id:
                raise ModelError("track id must not be empty")
            if track.id in track_ids:
                raise ModelError(f"duplicate track id: {track.id}")
            track_ids.add(track.id)
            for clip in track.clips:
                if not clip.id:
                    raise ModelError(f"track {track.id}: clip id must not be empty")
                if clip.id in clip_ids:
                    raise ModelError(f"duplicate clip id: {clip.id}")
                clip_ids.add(clip.id)
                if clip.track != track.id:
                    raise ModelError(
                        f"clip {clip.id}: track {clip.track!r} does not match {track.id!r}"
                    )
                values = {
                    "start_ns": clip.start_ns,
                    "duration_ns": clip.duration_ns,
                    "source_in_ns": clip.source_in_ns,
                }
                for name, value in values.items():
                    if isinstance(value, bool) or not isinstance(value, int):
                        raise ModelError(f"clip {clip.id}: {name} must be integer ns")
                    if value < 0:
                        raise ModelError(f"clip {clip.id}: {name} must not be negative")
                if clip.source_duration_ns is not None:
                    if isinstance(clip.source_duration_ns, bool) or not isinstance(
                        clip.source_duration_ns, int
                    ):
                        raise ModelError(f"clip {clip.id}: source duration must be integer ns")
                    if clip.source_duration_ns < 0:
                        raise ModelError(f"clip {clip.id}: source duration must not be negative")
        for transition in self.transitions:
            if transition.id in transition_ids:
                raise ModelError(f"duplicate transition id: {transition.id}")
            transition_ids.add(transition.id)
            if transition.duration_ns < 0:
                raise ModelError(f"transition {transition.id}: duration must not be negative")
            if transition.max_duration_ns is not None and transition.max_duration_ns < 0:
                raise ModelError(f"transition {transition.id}: max duration must not be negative")
        marker_ids: set[str] = set()
        for marker in self.markers:
            if marker.id in marker_ids:
                raise ModelError(f"duplicate marker id: {marker.id}")
            marker_ids.add(marker.id)
            if marker.start_ns < 0 or marker.duration_ns < 0:
                raise ModelError(f"marker {marker.id}: negative time")
        self.normalize_links()
        for track in self.tracks:
            track.clips.sort(key=lambda clip: (clip.start_ns, clip.id))
        self.transitions.sort(key=lambda item: (item.track, item.from_clip_id, item.id))
        self.link_groups.sort(key=lambda group: group.id)
        self.markers.sort(key=lambda marker: (marker.start_ns, marker.id))

    def clone(self) -> "Timeline":
        tracks = [
            Track(
                track.id,
                track.kind,
                [replace(clip, metadata=dict(clip.metadata)) for clip in track.clips],
                dict(track.metadata),
            )
            for track in self.tracks
        ]
        transitions = [
            replace(item, metadata=dict(item.metadata)) for item in self.transitions
        ]
        groups = [LinkGroup(group.id, list(group.clip_ids)) for group in self.link_groups]
        markers = [replace(marker, metadata=dict(marker.metadata)) for marker in self.markers]
        return Timeline(
            version=self.version,
            fps=self.fps,
            declared_duration_ns=self.declared_duration_ns,
            tracks=tracks,
            transitions=transitions,
            link_groups=groups,
            markers=markers,
            metadata=dict(self.metadata),
        )


def link_members(timeline: Timeline, clip: Clip) -> list[Clip]:
    """Return the clip and all linked peers in stable id order."""
    if not clip.link_id:
        return [clip]
    by_id = timeline.clip_by_id()
    members = {clip.id}
    for group in timeline.link_groups:
        if group.id == clip.link_id:
            members.update(group.clip_ids)
    return [by_id[clip_id] for clip_id in sorted(members) if clip_id in by_id]


def iter_clips(timeline: Timeline) -> Iterable[Clip]:
    for track in timeline.tracks:
        yield from track.clips
