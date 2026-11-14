"""Conflict resolution strategies with linked-clip cascade."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .conflicts import (
    LINK_MISALIGNED, OVERLAP, SOURCE_OVERRUN, TRANSITION_INVALID, ZERO_LENGTH,
    Conflict, scan_conflicts,
)
from .model import Clip, InvariantError, Timeline, link_members

POLICIES = ("ripple", "trim", "drop")


class ResolveError(ValueError):
    pass


class NonConvergenceError(ResolveError):
    def __init__(self, iterations: int, remaining: list[Conflict]):
        self.iterations = iterations
        self.remaining = remaining
        super().__init__(
            f"resolution did not converge after {iterations} iterations; "
            f"{len(remaining)} conflict(s) remain"
        )


@dataclass(slots=True)
class Resolution:
    action: str
    conflict: Conflict
    clip_ids: list[str] = field(default_factory=list)
    amount_ns: int = 0
    detail: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "action": self.action,
            "kind": self.conflict.kind,
            "track": self.conflict.track,
            "clip_ids": list(self.clip_ids),
            "amount_ns": self.amount_ns,
            "detail": self.detail,
        }


@dataclass(slots=True)
class ResolveResult:
    timeline: Timeline
    resolutions: list[Resolution]
    unresolved: list[Conflict]
    iterations: int
    fixed_point: bool
    policy: str

    @property
    def resolved(self) -> int:
        return len(self.resolutions)


def _clip_by_id(timeline: Timeline, clip_id: str) -> Clip | None:
    for track in timeline.tracks:
        for clip in track.clips:
            if clip.id == clip_id:
                return clip
    return None


def _remove_clips(timeline: Timeline, clip_ids: set[str]) -> list[str]:
    removed: list[str] = []
    for track in timeline.tracks:
        kept: list[Clip] = []
        for clip in track.clips:
            if clip.id in clip_ids:
                removed.append(clip.id)
            else:
                kept.append(clip)
        track.clips = kept
    for group in timeline.link_groups:
        group.clip_ids = [clip_id for clip_id in group.clip_ids if clip_id not in clip_ids]
    timeline.link_groups = [group for group in timeline.link_groups if group.clip_ids]
    timeline.transitions = [
        item for item in timeline.transitions
        if item.from_clip_id not in clip_ids and item.to_clip_id not in clip_ids
    ]
    return removed


def _remove_transition(timeline: Timeline, transition_id: str) -> bool:
    before = len(timeline.transitions)
    timeline.transitions = [item for item in timeline.transitions if item.id != transition_id]
    return len(timeline.transitions) != before


def _shift(timeline: Timeline, clip_ids: set[str], delta_ns: int, ripple: bool) -> list[str]:
    if delta_ns <= 0:
        return []
    affected = set(clip_ids)
    if ripple:
        clips = timeline.clip_by_id()
        for clip_id in list(affected):
            clip = clips.get(clip_id)
            if clip is None:
                continue
            for peer in timeline.track_by_id()[clip.track].clips:
                if peer.start_ns >= clip.start_ns:
                    affected.add(peer.id)
    moved: list[str] = []
    clips = timeline.clip_by_id()
    for clip_id in sorted(affected):
        clip = clips.get(clip_id)
        if clip is not None:
            clip.start_ns += delta_ns
            moved.append(clip.id)
    return moved


def _trim_cap(clips: list[Clip], wanted_ns: int) -> int:
    cap = max(0, wanted_ns)
    for clip in clips:
        if clip.source_duration_ns is not None:
            cap = min(cap, clip.source_remaining_ns or 0)
        cap = min(cap, clip.duration_ns)
    return cap


def _trim(timeline: Timeline, target: Clip, amount_ns: int) -> Resolution | None:
    members = link_members(timeline, target)
    cap = _trim_cap(members, amount_ns)
    if cap <= 0:
        return None
    for clip in members:
        clip.duration_ns -= cap
    return Resolution(
        "trim", Conflict(ZERO_LENGTH, target.track, target.id, "", 0),
        [clip.id for clip in members], cap, "trim linked tail",
    )


def _drop_victim(timeline: Timeline, clip_a: Clip, clip_b: Clip) -> list[str]:
    candidates = sorted(
        {clip_a.id, clip_b.id},
        key=lambda clip_id: (
            (_clip_by_id(timeline, clip_id).priority if _clip_by_id(timeline, clip_id) else 0),
            clip_id,
        ),
    )
    victim = _clip_by_id(timeline, candidates[0])
    if victim is None:
        return []
    members = {clip.id for clip in link_members(timeline, victim)}
    return _remove_clips(timeline, members)


def _apply_overlap(timeline: Timeline, conflict: Conflict, policy: str) -> Resolution | None:
    clip_a = _clip_by_id(timeline, conflict.a_id)
    clip_b = _clip_by_id(timeline, conflict.b_id)
    if clip_a is None or clip_b is None:
        return None
    earlier = min((clip_a, clip_b), key=lambda clip: (clip.start_ns, clip.id))
    later = max((clip_a, clip_b), key=lambda clip: (clip.start_ns, clip.id))
    if policy == "drop":
        removed = _drop_victim(timeline, clip_a, clip_b)
        return Resolution("drop", conflict, removed, conflict.overlap_ns, "drop lower priority")
    same_linked_track = (
        earlier.link_id is not None
        and earlier.link_id == later.link_id
        and clip_a.track == clip_b.track
    )
    if policy == "ripple" and not same_linked_track:
        ids = {clip.id for clip in link_members(timeline, later)}
        moved = _shift(timeline, ids, conflict.overlap_ns, ripple=True)
        if not moved:
            return None
        return Resolution("ripple", conflict, moved, conflict.overlap_ns, "shift downstream")
    result = _trim(timeline, earlier, conflict.overlap_ns)
    if result is None:
        return None
    result.conflict = conflict
    return result


def _apply_link(timeline: Timeline, conflict: Conflict) -> Resolution | None:
    by_id = timeline.clip_by_id()
    anchor = by_id.get(conflict.a_id)
    target = by_id.get(conflict.b_id)
    if anchor is None or target is None or not target.link_id:
        return None
    group = [by_id[clip_id] for clip_id in sorted(
        next((item.clip_ids for item in timeline.link_groups if item.id == target.link_id), [])
    ) if clip_id in by_id]
    if len(group) < 2:
        return None
    if anchor.start_ns == target.start_ns:
        return None
    shift = anchor.start_ns - target.start_ns
    moved: list[str] = []
    for clip in group:
        clip.start_ns = anchor.start_ns
        moved.append(clip.id)
    return Resolution("align", conflict, moved, abs(shift), "align linked starts")


def _trim_source(timeline: Timeline, conflict: Conflict) -> Resolution | None:
    clip = _clip_by_id(timeline, conflict.a_id)
    if clip is None or clip.source_duration_ns is None:
        return None
    allowable = max(0, clip.source_duration_ns - clip.source_in_ns)
    if allowable >= clip.duration_ns:
        return None
    members = link_members(timeline, clip)
    cap = allowed = allowable
    for member in members:
        if member.source_duration_ns is None:
            cap = min(cap, member.duration_ns)
        else:
            cap = min(cap, max(0, member.source_duration_ns - member.source_in_ns))
    cap = min(cap, clip.duration_ns)
    trim = clip.duration_ns - cap
    if trim <= 0:
        return None
    for member in members:
        if member.source_duration_ns is not None:
            member.duration_ns = min(member.duration_ns, cap)
        else:
            member.duration_ns = min(member.duration_ns, cap)
    return Resolution("trim", conflict, [member.id for member in members], trim,
                      f"source allowable {allowed} ns")


def _apply_transition(timeline: Timeline, conflict: Conflict, policy: str) -> Resolution | None:
    by_id = timeline.clip_by_id()
    if conflict.kind == TRANSITION_INVALID:
        trans = next(
            (item for item in timeline.transitions
             if item.from_clip_id == conflict.a_id and item.to_clip_id == conflict.b_id),
            None,
        )
        if trans is None:
            return None
        clip_a = by_id.get(trans.from_clip_id)
        clip_b = by_id.get(trans.to_clip_id)
        if clip_a is None or clip_b is None or clip_a.track != trans.track or clip_b.track != trans.track:
            if _remove_transition(timeline, trans.id):
                return Resolution("drop_transition", conflict, [], 0, "remove invalid transition")
            return None
        if clip_a.start_ns >= clip_b.start_ns or trans.duration_ns <= 0:
            if _remove_transition(timeline, trans.id):
                return Resolution("drop_transition", conflict, [], 0, "remove invalid transition")
            return None
        cap = min(clip_a.duration_ns, clip_b.duration_ns)
        if trans.max_duration_ns is not None:
            cap = min(cap, trans.max_duration_ns)
        if trans.duration_ns > cap:
            if cap <= 0:
                _remove_transition(timeline, trans.id)
                return Resolution("drop_transition", conflict, [], 0, "transition cannot fit")
            old = trans.duration_ns
            trans.duration_ns = cap
            return Resolution("clamp_transition", conflict, [], old - cap, "clamp to sides/max")
        overlap = max(0, clip_a.end_ns - clip_b.start_ns)
        excess = overlap - trans.duration_ns
        if excess > 0:
            overlap_conflict = Conflict(
                OVERLAP, conflict.track, clip_a.id, clip_b.id, excess,
                clip_b.start_ns, "overlap beyond transition",
            )
            return _apply_overlap(timeline, overlap_conflict, policy)
        return None
    return None


def _resolve_one(timeline: Timeline, conflict: Conflict, policy: str) -> Resolution | None:
    if conflict.kind == ZERO_LENGTH:
        removed = _remove_clips(timeline, {conflict.a_id})
        return Resolution("drop_zero", conflict, removed, 0, "remove zero-length clip") if removed else None
    if conflict.kind == LINK_MISALIGNED:
        return _apply_link(timeline, conflict)
    if conflict.kind == SOURCE_OVERRUN:
        if policy == "trim":
            return _trim_source(timeline, conflict)
        clip = _clip_by_id(timeline, conflict.a_id)
        if clip is not None:
            removed = _remove_clips(timeline, {member.id for member in link_members(timeline, clip)})
            return Resolution("drop_source", conflict, removed, conflict.overlap_ns,
                              "drop source overrun") if removed else None
        return None
    if conflict.kind == TRANSITION_INVALID:
        return _apply_transition(timeline, conflict, policy)
    if conflict.kind == OVERLAP:
        return _apply_overlap(timeline, conflict, policy)
    return None


def resolve(timeline: Timeline, policy: str = "ripple", *,
            max_iterations: int = 10_000) -> ResolveResult:
    if policy not in POLICIES:
        raise ResolveError(f"unknown policy {policy!r}; expected one of {', '.join(POLICIES)}")
    if max_iterations <= 0:
        raise ResolveError("max_iterations must be positive")
    work = timeline.clone()
    work.validate()
    resolutions: list[Resolution] = []
    iterations = 0
    last: list[Conflict] = scan_conflicts(work)
    while last:
        if iterations >= max_iterations:
            raise NonConvergenceError(iterations, last)
        action = _resolve_one(work, last[0], policy)
        iterations += 1
        if action is None:
            break
        action.conflict = last[0]
        resolutions.append(action)
        last = scan_conflicts(work)
    try:
        work.validate()
    except Exception as exc:
        if isinstance(exc, InvariantError):
            raise
        raise InvariantError(f"resolved timeline failed validation: {exc}") from exc
    fixed_point = not last
    if not fixed_point and iterations >= max_iterations:
        raise NonConvergenceError(iterations, last)
    return ResolveResult(work, resolutions, last, iterations, fixed_point, policy)


resolve_timeline = resolve
resolve_conflicts = resolve
