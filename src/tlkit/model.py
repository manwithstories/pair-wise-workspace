"""Core timeline model on an integer project frame grid.

All time arithmetic uses ``fractions.Fraction``; floats are never used.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

CLIP = "clip"
GAP = "gap"
TRANSITION = "transition"
VIDEO = "video"
AUDIO = "audio"


@dataclass(frozen=True)
class TimeRange:
    """Half-open interval [start, start+duration) in project frames."""

    start: int
    duration: int

    @property
    def end(self) -> int:
        return self.start + self.duration

    def overlaps(self, other: "TimeRange") -> bool:
        return self.start < other.end and other.start < self.end

    def intersection(self, other: "TimeRange") -> "TimeRange | None":
        lo = max(self.start, other.start)
        hi = min(self.end, other.end)
        if lo >= hi:
            return None
        return TimeRange(lo, hi - lo)


@dataclass
class Clip:
    """A timeline item: real clip, gap, or transition."""

    id: str
    range: TimeRange
    kind: str = CLIP
    name: str = ""
    enabled: bool = True
    muted: bool = False
    left: str | None = None   # transition: id of the clip on the left
    right: str | None = None  # transition: id of the clip on the right

    @property
    def visible(self) -> bool:
        return self.kind == CLIP and self.enabled and not self.muted


@dataclass
class Track:
    name: str
    kind: str = VIDEO
    items: list[Clip] = field(default_factory=list)

    def clips(self) -> list[Clip]:
        return [c for c in self.items if c.kind == CLIP]

    def find(self, clip_id: str) -> Clip | None:
        for c in self.items:
            if c.id == clip_id:
                return c
        return None


@dataclass
class Timeline:
    name: str
    fps: Fraction
    tracks: list[Track] = field(default_factory=list)

    @property
    def duration(self) -> int:
        """Derived from enabled clips and (trailing) gaps; transitions ignored."""
        dur = 0
        for tr in self.tracks:
            for c in tr.items:
                if c.kind == GAP or (c.kind == CLIP and c.enabled):
                    dur = max(dur, c.range.end)
        return dur

    def find_clip(self, clip_id: str) -> tuple[Track, Clip] | None:
        for tr in self.tracks:
            c = tr.find(clip_id)
            if c is not None:
                return tr, c
        return None

    def clip_count(self) -> int:
        return sum(len(tr.clips()) for tr in self.tracks)


def round_half_up(x: Fraction) -> int:
    floor = x.numerator // x.denominator
    if x - floor >= Fraction(1, 2):
        return floor + 1
    return floor


def rational_to_frames(value: int, rate: int, fps: Fraction) -> tuple[int, Fraction]:
    """Convert ``value/rate`` seconds to the nearest project frame.

    Returns ``(frame, remainder)`` where ``remainder`` is the exact Fraction
    offset (in frames) discarded by rounding; zero when the conversion is
    exact. Never uses floats.
    """
    exact = Fraction(value, rate) * fps
    frame = round_half_up(exact)
    return frame, exact - frame


def frames_to_seconds(frames: int, fps: Fraction) -> Fraction:
    return Fraction(frames, 1) / fps


def canonical(timeline: Timeline) -> dict:
    """Deterministic JSON-able snapshot used for hashing and cycle checks."""
    return {
        "name": timeline.name,
        "fps": f"{timeline.fps.numerator}/{timeline.fps.denominator}",
        "tracks": [
            {
                "name": tr.name,
                "kind": tr.kind,
                "items": [
                    {
                        "id": c.id,
                        "kind": c.kind,
                        "start": c.range.start,
                        "duration": c.range.duration,
                        "enabled": c.enabled,
                        "muted": c.muted,
                        "left": c.left,
                        "right": c.right,
                    }
                    for c in sorted(tr.items, key=lambda c: (c.range.start, c.id))
                ],
            }
            for tr in timeline.tracks
        ],
    }
