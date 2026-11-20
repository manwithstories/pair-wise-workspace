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
