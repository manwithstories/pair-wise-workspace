"""Single-track sweep-line conflict detection.

Cross-track clips never conflict merely because they occupy the same time.
Only link groups add a cross-track rule: every member starts together.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable

from .model import Clip, Timeline, Transition

OVERLAP = "overlap"
ZERO_LENGTH = "zero_length"
SOURCE_OVERRUN = "source_overrun"
LINK_MISALIGNED = "link_misaligned"
TRANSITION_INVALID = "transition_invalid"
CONFLICT_KINDS = (
    OVERLAP,
    ZERO_LENGTH,
    SOURCE_OVERRUN,
    LINK_MISALIGNED,
    TRANSITION_INVALID,
)


@dataclass(slots=True)
class Conflict:
    kind: str
    track: str
    a_id: str
    b_id: str
    overlap_ns: int
    at_ns: int = 0
    message: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind,
            "track": self.track,
            "a_id": self.a_id,
            "b_id": self.b_id,
            "overlap_ns": self.overlap_ns,
            "at_ns": self.at_ns,
            "message": self.message,
        }

    def key(self) -> tuple[Any, ...]:
        return (self.track, self.at_ns, self.kind, self.a_id, self.b_id, self.overlap_ns)


def _transition_allowance(item: Transition, clip_a: Clip, clip_b: Clip) -> tuple[bool, str]:
    if item.duration_ns <= 0:
        return False, "transition duration must be positive"
    if item.duration_ns > clip_a.duration_ns:
        return False, f"transition is longer than source clip {clip_a.id}"
    if item.duration_ns > clip_b.duration_ns:
        return False, f"transition is longer than target clip {clip_b.id}"
    if item.max_duration_ns is not None and item.duration_ns > item.max_duration_ns:
        return False, "transition exceeds its explicit maximum"
    return True, ""


def _validate_transition(
    item: Transition, by_id: dict[str, Clip], overlaps: dict[tuple[str, str], int]
) -> Conflict | None:
    clip_a = by_id.get(item.from_clip_id)
    clip_b = by_id.get(item.to_clip_id)
    if clip_a is None or clip_b is None:
        missing = item.from_clip_id if clip_a is None else item.to_clip_id
        return Conflict(
            TRANSITION_INVALID, item.track, item.from_clip_id, item.to_clip_id, 0,
            clip_a.start_ns if clip_a else clip_b.start_ns if clip_b else 0,
            f"transition references missing clip {missing}",
        )
    if clip_a.track != item.track or clip_b.track != item.track:
        return Conflict(
            TRANSITION_INVALID, item.track, clip_a.id, clip_b.id, 0,
            min(clip_a.start_ns, clip_b.start_ns), "transition crosses tracks",
        )
    if clip_a.start_ns >= clip_b.start_ns:
        return Conflict(
            TRANSITION_INVALID, item.track, clip_a.id, clip_b.id, 0,
            min(clip_a.start_ns, clip_b.start_ns),
            "transition source must start before target",
        )
    allowed, reason = _transition_allowance(item, clip_a, clip_b)
    actual = overlaps.get((item.from_clip_id, item.to_clip_id), 0)
    if not allowed:
        return Conflict(
            TRANSITION_INVALID, item.track, clip_a.id, clip_b.id, actual,
            max(clip_a.start_ns, clip_b.start_ns), reason,
        )
    if actual > item.duration_ns:
        return Conflict(
            TRANSITION_INVALID, item.track, clip_a.id, clip_b.id,
            actual - item.duration_ns, max(clip_a.start_ns, clip_b.start_ns),
            "clip overlap exceeds transition duration",
        )
    if actual > min(clip_a.duration_ns, clip_b.duration_ns):
        return Conflict(
            TRANSITION_INVALID, item.track, clip_a.id, clip_b.id, actual,
            max(clip_a.start_ns, clip_b.start_ns),
            "clip overlap exceeds one side's duration",
        )
    return None


def _track_conflicts(track_id: str, clips: Iterable[Clip],
                     transitions_by_pair: dict[tuple[str, str], Transition]
                     ) -> tuple[list[Conflict], dict[tuple[str, str], int]]:
    ordered = sorted(clips, key=lambda clip: (clip.start_ns, clip.id))
    ordered_by_id = {clip.id: clip for clip in ordered}
    conflicts: list[Conflict] = []
    overlap_pairs: dict[tuple[str, str], int] = {}
    active: list[Clip] = []
    for clip in ordered:
        if clip.duration_ns == 0:
            conflicts.append(Conflict(
                ZERO_LENGTH, track_id, clip.id, "", 0, clip.start_ns,
                "clip has zero duration",
            ))
        active = [item for item in active if item.end_ns > clip.start_ns]
        for other in active:
            start = max(other.start_ns, clip.start_ns)
            end = min(other.end_ns, clip.end_ns)
            if end > start:
                overlap_pairs[(other.id, clip.id)] = end - start
        active.append(clip)
    for (a_id, b_id), overlap in overlap_pairs.items():
        transition = transitions_by_pair.get((a_id, b_id))
        if transition is None:
            clip_a = ordered_by_id[a_id]
            clip_b = ordered_by_id[b_id]
            conflicts.append(Conflict(
                OVERLAP, track_id, a_id, b_id, overlap,
                max(clip_a.start_ns, clip_b.start_ns), "same-track clips overlap",
            ))
    return conflicts, overlap_pairs


def scan_conflicts(timeline: Timeline) -> list[Conflict]:
    """Return all editorial conflicts in deterministic order."""
    by_id = timeline.clip_by_id()
    transitions_by_pair = {
        (item.from_clip_id, item.to_clip_id): item for item in timeline.transitions
    }
    conflicts: list[Conflict] = []
    overlap_pairs: dict[tuple[str, str], int] = {}
    for track in timeline.tracks:
        track_conflicts, track_pairs = _track_conflicts(
            track.id, track.clips, transitions_by_pair
        )
        conflicts.extend(track_conflicts)
        overlap_pairs.update(track_pairs)
    for item in timeline.transitions:
        conflict = _validate_transition(item, by_id, overlap_pairs)
        if conflict is not None:
            conflicts.append(conflict)
    for clip in by_id.values():
        available = clip.source_duration_ns
        if available is not None and clip.source_out_ns > available:
            conflicts.append(Conflict(
                SOURCE_OVERRUN, clip.track, clip.id, "source",
                clip.source_out_ns - available, clip.start_ns,
                f"source out {clip.source_out_ns} exceeds available {available} ns",
            ))
    for group in sorted(timeline.link_groups, key=lambda item: item.id):
        members = [by_id[clip_id] for clip_id in group.clip_ids if clip_id in by_id]
        if len(members) < 2:
            continue
        anchor = min(members, key=lambda clip: (clip.start_ns, clip.id))
        for clip in sorted(members, key=lambda item: item.id):
            if clip.start_ns != anchor.start_ns:
                conflicts.append(Conflict(
                    LINK_MISALIGNED, clip.track, anchor.id, clip.id,
                    abs(clip.start_ns - anchor.start_ns), min(clip.start_ns, anchor.start_ns),
                    f"link group {group.id} starts are not aligned",
                ))
    return sorted(conflicts, key=Conflict.key)


def by_kind(conflicts: Iterable[Conflict]) -> dict[str, int]:
    result = {kind: 0 for kind in CONFLICT_KINDS}
    for conflict in conflicts:
        result[conflict.kind] = result.get(conflict.kind, 0) + 1
    return result


def conflict_dicts(conflicts: Iterable[Conflict]) -> list[dict[str, Any]]:
    return [conflict.to_dict() for conflict in conflicts]


detect_conflicts = scan_conflicts
summarize = by_kind
