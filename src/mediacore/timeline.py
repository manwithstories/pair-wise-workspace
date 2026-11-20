"""Explicit clip/transition specifications.

Half-open ``[start_us, end_us)`` ranges on the **source** timeline, expressed
in integer microseconds.  Exact source tick bounds are derived from the
track's rational timebase by :meth:`Timeline.resolve`, never from floats.

Two clips on the same track may not overlap unless a declared transition
licenses exactly that overlap; the transition then becomes a composite node
depending on both clips, so a failure on either side isolates the blend.

Every violation is a :class:`~mediacore.errors.TimelineError` carrying the
offending ids and interval, so an operator can jump straight to the clip
rather than bisecting the edit.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable, Literal

from .errors import ParameterError, TimelineError
from .timebase import Timebase, Us

__all__ = [
    "TrackSpec",
    "ClipSpec",
    "TransitionSpec",
    "ResolvedClip",
    "Timeline",
]


TrackKind = Literal["video", "audio"]


@dataclass(frozen=True)
class TrackSpec:
    """One input track: a file, a stream selector and its timing metadata."""

    id: str
    path: str
    kind: TrackKind
    stream_index: int | None = None
    timebase: str = "1/1000"
    origin_us: int = 0
    wrap_at: int | None = None
    sample_rate: int | None = None
    label: str = ""

    def __post_init__(self) -> None:
        if not self.id:
            raise ParameterError("track id must be non-empty", kind="track")
        if self.kind not in ("video", "audio"):
            raise ParameterError("track kind must be video or audio", track=self.id, kind=self.kind)
        if self.origin_us < 0:
            # Negative origins are legal in the *source*; they are normalised
            # away by the probe.  A declarative origin cannot be negative.
            raise TimelineError(
                "track origin_us must be >= 0",
                code="timeline.bad_origin",
                track=self.id,
                origin_us=self.origin_us,
            )
        Timebase.parse(self.timebase)  # validate eagerly

    @property
    def tb(self) -> Timebase:
        return Timebase.parse(self.timebase)

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "path": self.path,
            "kind": self.kind,
            "stream_index": self.stream_index,
            "timebase": self.timebase,
            "origin_us": self.origin_us,
            "wrap_at": self.wrap_at,
            "sample_rate": self.sample_rate,
            "label": self.label,
        }


@dataclass(frozen=True)
class ClipSpec:
    """An explicit half-open range ``[start_us, end_us)`` on one track."""

    id: str
    track: str
    start_us: Us
    end_us: Us
    gain_db: float = 0.0
    label: str = ""

    def __post_init__(self) -> None:
        if not self.id:
            raise ParameterError("clip id must be non-empty", kind="clip")
        if not self.track:
            raise ParameterError("clip must reference a track", clip=self.id)
        if self.end_us <= self.start_us:
            raise TimelineError(
                "clip is empty or reversed",
                code="timeline.empty_clip",
                clip=self.id,
                track=self.track,
                start_us=self.start_us,
                end_us=self.end_us,
                duration_us=self.end_us - self.start_us,
            )

    @property
    def duration_us(self) -> Us:
        return self.end_us - self.start_us

    def overlaps(self, other: "ClipSpec") -> int:
        """Overlap length in microseconds (0 when disjoint)."""
        return max(0, min(self.end_us, other.end_us) - max(self.start_us, other.start_us))

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "track": self.track,
            "start_us": self.start_us,
            "end_us": self.end_us,
            "gain_db": self.gain_db,
            "label": self.label,
        }


@dataclass(frozen=True)
class TransitionSpec:
    """A blend between two clips; the blend interval is the composite node."""

    id: str
    left: str
    right: str
    duration_us: Us
    curve: Literal["linear", "equal_power"] = "linear"
    # Optional extra ordering dependencies on other transition ids.  Used by
    # the cycle fixture and by real "this blend follows that one" chains.
    after: tuple[str, ...] = ()

    def __post_init__(self) -> None:
        if not self.id:
            raise ParameterError("transition id must be non-empty", kind="transition")
        if self.left == self.right:
            raise TimelineError(
                "transition needs two distinct clips",
                code="timeline.transition_self",
                transition=self.id,
                clip=self.left,
            )
        if self.duration_us <= 0:
            raise TimelineError(
                "transition duration must be positive",
                code="timeline.empty_transition",
                transition=self.id,
                duration_us=self.duration_us,
            )
        if self.id in self.after:
            raise TimelineError(
                "transition cannot depend on itself",
                code="timeline.transition_self_dependency",
                transition=self.id,
            )
        if self.curve not in ("linear", "equal_power"):
            raise ParameterError(
                "unknown transition curve", transition=self.id, curve=self.curve
            )

    @property
    def node_id(self) -> str:
        return f"transition:{self.id}"

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "left": self.left,
            "right": self.right,
            "duration_us": self.duration_us,
            "curve": self.curve,
            "after": list(self.after),
        }


@dataclass(frozen=True)
class ResolvedClip:
    """A clip with exact source tick bounds derived from its track timebase."""

    clip: ClipSpec
    track: TrackSpec
    start_tick: int
    end_tick: int
    start_us: Us
    end_us: Us

    @property
    def duration_us(self) -> Us:
        return self.end_us - self.start_us


@dataclass
class Timeline:
    """Validated collection of tracks, clips and transitions."""

    tracks: dict[str, TrackSpec] = field(default_factory=dict)
    clips: dict[str, ClipSpec] = field(default_factory=dict)
    transitions: dict[str, TransitionSpec] = field(default_factory=dict)

    # -- construction -------------------------------------------------------
    def add_track(self, track: TrackSpec) -> None:
        if track.id in self.tracks:
            raise TimelineError(
                "duplicate track id", code="timeline.duplicate_id", track=track.id
            )
        self.tracks[track.id] = track

    def add_clip(self, clip: ClipSpec) -> None:
        if clip.id in self.clips:
            raise TimelineError(
                "duplicate clip id", code="timeline.duplicate_id", clip=clip.id
            )
        self.clips[clip.id] = clip

    def add_transition(self, transition: TransitionSpec) -> None:
        if transition.id in self.transitions:
            raise TimelineError(
                "duplicate transition id",
                code="timeline.duplicate_id",
                transition=transition.id,
            )
        self.transitions[transition.id] = transition

    # -- serialisation ------------------------------------------------------
    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Timeline":
        tl = cls()
        for t in data.get("tracks", []):
            tl.add_track(TrackSpec(**t))
        for c in data.get("clips", []):
            tl.add_clip(ClipSpec(**c))
        for t in data.get("transitions", []):
            tl.add_transition(TransitionSpec(**t))
        tl.validate()
        return tl

    @classmethod
    def load(cls, path: str | Path) -> "Timeline":
        try:
            data = json.loads(Path(path).read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise ParameterError(
                "cannot read timeline", path=str(path), reason=str(exc)
            ) from exc
        if not isinstance(data, dict):
            raise ParameterError("timeline must be a JSON object", path=str(path))
        return cls.from_dict(data)

    def to_dict(self) -> dict[str, Any]:
        return {
            "tracks": [t.to_dict() for t in self.tracks.values()],
            "clips": [c.to_dict() for c in self.clips.values()],
            "transitions": [t.to_dict() for t in self.transitions.values()],
        }

    # -- validation ---------------------------------------------------------
    def validate(self, *, durations_us: dict[str, int] | None = None) -> "Timeline":
        """Check referential integrity, ranges, overlaps and transitions.

        ``durations_us`` maps track id -> measured source duration.  When
        supplied, every clip is checked against the real material; when it is
        not, only declarative invariants are enforced (useful for early CLI
        argument validation before probes run).
        """
        self._check_references(durations_us)
        self._check_overlaps()
        self._check_transitions()
        return self

    def _check_references(self, durations_us: dict[str, int] | None) -> None:
        for clip in self.clips.values():
            track = self.tracks.get(clip.track)
            if track is None:
                raise TimelineError(
                    "clip references unknown track",
                    code="timeline.unknown_track",
                    clip=clip.id,
                    track=clip.track,
                )
            if clip.start_us < 0:
                raise TimelineError(
                    "clip starts before source origin",
                    code="timeline.out_of_range",
                    clip=clip.id,
                    track=track.id,
                    start_us=clip.start_us,
                )
            if durations_us is not None:
                limit = durations_us.get(track.id)
                if limit is None:
                    raise TimelineError(
                        "no measured duration for track",
                        code="timeline.unknown_duration",
                        clip=clip.id,
                        track=track.id,
                    )
                if clip.end_us > limit:
                    raise TimelineError(
                        "clip exceeds source duration",
                        code="timeline.out_of_range",
                        clip=clip.id,
                        track=track.id,
                        end_us=clip.end_us,
                        source_duration_us=limit,
                        overshoot_us=clip.end_us - limit,
                    )

    def _check_overlaps(self) -> None:
        by_track: dict[str, list[ClipSpec]] = {}
        for clip in self.clips.values():
            by_track.setdefault(clip.track, []).append(clip)
        licensed = {
            frozenset((t.left, t.right)): t for t in self.transitions.values()
        }
        for track_id, clips in by_track.items():
            ordered = sorted(clips, key=lambda c: (c.start_us, c.end_us, c.id))
            for i, left in enumerate(ordered):
                for right in ordered[i + 1 :]:
                    overlap = left.overlaps(right)
                    if overlap <= 0:
                        continue
                    transition = licensed.get(frozenset((left.id, right.id)))
                    if transition is None:
                        raise TimelineError(
                            "clips overlap without a transition",
                            code="timeline.overlap",
                            track=track_id,
                            left=left.id,
                            right=right.id,
                            overlap_us=overlap,
                        )
                    if overlap > transition.duration_us:
                        raise TimelineError(
                            "overlap is longer than its transition",
                            code="timeline.overlap_unlicensed",
                            track=track_id,
                            left=left.id,
                            right=right.id,
                            overlap_us=overlap,
                            transition=transition.id,
                            transition_us=transition.duration_us,
                        )

    def _check_transitions(self) -> None:
        for trans in self.transitions.values():
            for dep in trans.after:
                if dep not in self.transitions:
                    raise TimelineError(
                        "transition depends on unknown transition",
                        code="timeline.unknown_transition",
                        transition=trans.id,
                        dependency=dep,
                    )
            left = self.clips.get(trans.left)
            right = self.clips.get(trans.right)
            for missing, name in ((left, trans.left), (right, trans.right)):
                if missing is None:
                    raise TimelineError(
                        "transition references unknown clip",
                        code="timeline.unknown_clip",
                        transition=trans.id,
                        clip=name,
                    )
            assert left is not None and right is not None
            for clip in (left, right):
                if clip.duration_us < trans.duration_us:
                    raise TimelineError(
                        "transition is longer than its clip",
                        code="timeline.transition_too_long",
                        transition=trans.id,
                        clip=clip.id,
                        clip_us=clip.duration_us,
                        transition_us=trans.duration_us,
                    )

    # -- resolution ---------------------------------------------------------
    def resolve(self) -> list[ResolvedClip]:
        """Derive exact source tick bounds for every clip."""
        out: list[ResolvedClip] = []
        for clip in self.clips.values():
            track = self.tracks[clip.track]
            tb = track.tb
            start_tick = tb.from_us_floor(clip.start_us)
            end_tick = tb.from_us_ceil(clip.end_us)
            out.append(
                ResolvedClip(
                    clip=clip,
                    track=track,
                    start_tick=start_tick,
                    end_tick=end_tick,
                    start_us=tb.to_us(start_tick),
                    end_us=tb.to_us(end_tick),
                )
            )
        out.sort(key=lambda rc: (rc.track.id, rc.start_tick, rc.clip.id))
        return out

    def clips_on(self, track_id: str) -> list[ClipSpec]:
        return sorted(
            (c for c in self.clips.values() if c.track == track_id),
            key=lambda c: (c.start_us, c.id),
        )

    def fingerprint(self) -> str:
        import hashlib

        payload = json.dumps(self.to_dict(), sort_keys=True, separators=(",", ":"))
        return hashlib.sha256(payload.encode("utf-8")).hexdigest()

    # -- convenience --------------------------------------------------------
