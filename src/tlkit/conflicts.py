"""Sweep-line conflict detection over the timeline model.

Same-track and cross-track overlap scans run in O(n log n + k) where k is
the number of reported pairs: items are sorted by start frame, then swept
with an active set pruned by end frame.
"""
from __future__ import annotations

from dataclasses import dataclass

from .model import CLIP, TRANSITION, VIDEO, Clip, Timeline, Track

ZERO_LENGTH = "zero_length"
NEGATIVE_START = "negative_start"
SAME_TRACK = "same_track_overlap"
CROSS_TRACK = "cross_track_overlap"
TRANSITION_ORPHAN = "transition_orphan"


@dataclass(frozen=True)
class Conflict:
    kind: str
    track: str
    clips: tuple[str, ...]
    start: int | None = None
    end: int | None = None
    detail: str = ""

    def to_json(self) -> dict:
        return {
            "kind": self.kind,
            "track": self.track,
            "clips": list(self.clips),
            "start": self.start,
            "end": self.end,
            "detail": self.detail,
        }


def _overlap_pairs(items: list[tuple[Clip, object]]) -> list[tuple[Clip, Clip]]:
    """Sweep-line pair enumeration. items: (clip, track) sorted by start."""
    pairs: list[tuple[Clip, Clip]] = []
    active: list[Clip] = []
    for clip, _ in items:
        active = [a for a in active if a.range.end > clip.range.start]
        for other in active:
            inter = other.range.intersection(clip.range)
            if inter is not None:
                pairs.append((other, clip))
        active.append(clip)
    return pairs


def detect(timeline: Timeline) -> list[Conflict]:
    out: list[Conflict] = []

    # structural defects + same-track sweep
    for tr in timeline.tracks:
        live = [c for c in tr.clips() if c.enabled]
        for c in live:
            if c.range.duration <= 0:
                out.append(Conflict(ZERO_LENGTH, tr.name, (c.id,),
                                    c.range.start, c.range.end, "duration <= 0"))
            if c.range.start < 0:
                out.append(Conflict(NEGATIVE_START, tr.name, (c.id,),
                                    c.range.start, c.range.end, "start < 0"))
        sweepable = sorted(
            (c for c in live if c.range.duration > 0),
            key=lambda c: (c.range.start, c.id),
        )
        for a, b in _overlap_pairs([(c, tr) for c in sweepable]):
            inter = a.range.intersection(b.range)
            out.append(Conflict(SAME_TRACK, tr.name, (a.id, b.id),
                                inter.start, inter.end))

    # cross-track video visibility sweep (enabled, unmuted clips only)
    visible: list[tuple[Clip, Track]] = []
    for tr in timeline.tracks:
        if tr.kind != VIDEO:
            continue
        for c in tr.clips():
            if c.visible and c.range.duration > 0:
                visible.append((c, tr))
    visible.sort(key=lambda ct: (ct[0].range.start, ct[0].id))
    track_of = {id(c): tr for c, tr in visible}
    seen_pairs: set[tuple[str, str]] = set()
    for a, b in _overlap_pairs(visible):
        ta, tb = track_of[id(a)], track_of[id(b)]
        if ta is tb:
            continue
        key = tuple(sorted((a.id, b.id)))
        if key in seen_pairs:
            continue
        seen_pairs.add(key)
        inter = a.range.intersection(b.range)
        label = "|".join(sorted((ta.name, tb.name)))
        out.append(Conflict(CROSS_TRACK, label, key, inter.start, inter.end))

    # transition adjacency: both referenced clips must exist, be enabled,
    # and sit flush against the transition range on the same track
    for tr in timeline.tracks:
        enabled = {c.id: c for c in tr.clips() if c.enabled}
        for t in tr.items:
            if t.kind != TRANSITION:
                continue
            left = enabled.get(t.left or "")
            right = enabled.get(t.right or "")
            ok = (
                left is not None and right is not None
                and left.range.end == t.range.start
                and right.range.start == t.range.end
            )
            if not ok:
                out.append(Conflict(TRANSITION_ORPHAN, tr.name, (t.id,),
                                    t.range.start, t.range.end,
                                    f"left={t.left} right={t.right}"))

    out.sort(key=lambda c: (c.kind, c.track, c.clips,
                            c.start if c.start is not None else 0))
    return out
