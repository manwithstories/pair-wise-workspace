"""Deterministic conflict resolution with cascade (fixpoint) iteration.

Policies: ``track-priority`` (upper track wins), ``later-in`` (later start
wins), ``trim-left`` / ``trim-right`` (preferred trim side), ``shift``
(move the loser right past the winner). Ties break by clip id ascending.
Each iteration fixes exactly one conflict; new overlaps created by trims
or shifts are re-detected until a fixpoint, an iteration cap, or a state
cycle is hit.
"""
from __future__ import annotations

import copy
import hashlib
import json
from dataclasses import dataclass, field

from .conflicts import (
    CROSS_TRACK, NEGATIVE_START, SAME_TRACK, TRANSITION_ORPHAN, ZERO_LENGTH,
    Conflict, detect,
)
from .model import Clip, TimeRange, Timeline, canonical

POLICIES = ("track-priority", "later-in", "trim-left", "trim-right", "shift")
DEFAULT_MAX_ITER = 512


@dataclass
class ResolveResult:
    timeline: Timeline
    decisions: list[dict] = field(default_factory=list)
    detected: int = 0
    unresolved: int = 0
    iterations: int = 0
    cycle: bool = False


def _state_hash(tl: Timeline) -> str:
    blob = json.dumps(canonical(tl), sort_keys=True).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()


def _pick_winner(tl: Timeline, a: Clip, b: Clip, policy: str) -> tuple[Clip, Clip]:
    idx = {id(c): i for i, tr in enumerate(tl.tracks) for c in tr.items}
    if policy == "track-priority" and idx[id(a)] != idx[id(b)]:
        return (a, b) if idx[id(a)] < idx[id(b)] else (b, a)
    if a.range.start != b.range.start:  # later-in wins
        return (a, b) if a.range.start > b.range.start else (b, a)
    return (a, b) if a.id < b.id else (b, a)  # deterministic tie-break


def _unique_id(tl: Timeline, base: str) -> str:
    candidate, n = base, 2
    while tl.find_clip(candidate) is not None:
        candidate, n = f"{base}{n}", n + 1
    return candidate


def _trim(tl: Timeline, loser: Clip, ov_start: int, ov_end: int,
          policy: str, decisions: list[dict], iteration: int) -> str:
    start, end = loser.range.start, loser.range.end
    can_left = ov_end < end
    can_right = ov_start > start
    if not can_left and not can_right:
        loser.enabled = False
        return "disable(fully covered)"
    if policy == "trim-left":
        side = "left" if can_left else "right"
    elif policy == "trim-right":
        side = "right" if can_right else "left"
    elif ov_start <= start and can_left:
        side = "left"
    elif ov_end >= end and can_right:
        side = "right"
    elif can_left and can_right:
        track, _ = tl.find_clip(loser.id)
        right = Clip(id=_unique_id(tl, f"{loser.id}~r"), kind=loser.kind,
                     name=loser.name, enabled=loser.enabled, muted=loser.muted,
                     range=TimeRange(ov_end, end - ov_end))
        loser.range = TimeRange(start, ov_start - start)
        track.items.append(right)
        return f"split(keep {loser.id}, spawn {right.id})"
    else:
        side = "left" if can_left else "right"
    if side == "left":
        loser.range = TimeRange(ov_end, end - ov_end)
        return f"trim-left(new start {ov_end})"
    loser.range = TimeRange(start, ov_start - start)
    return f"trim-right(new end {ov_start})"


def _fix_overlap(tl: Timeline, conflict: Conflict, policy: str,
                 decisions: list[dict], iteration: int) -> None:
    _, a = tl.find_clip(conflict.clips[0])
    _, b = tl.find_clip(conflict.clips[1])
    if a is None or b is None:
        return
    inter = a.range.intersection(b.range)
    if inter is None:
        return
    same_track = conflict.kind == SAME_TRACK
    if policy == "shift" and same_track:
        # the earlier clip keeps its slot; the later one ripples right,
        # which is monotone and cannot ping-pong
        if a.range.start != b.range.start:
            winner, loser = (a, b) if a.range.start < b.range.start else (b, a)
        else:
            winner, loser = (a, b) if a.id < b.id else (b, a)
        loser.range = TimeRange(winner.range.end, loser.range.duration)
        action = f"shift(after {winner.id} @ {winner.range.end})"
    else:
        winner, loser = _pick_winner(tl, a, b, policy)
        action = _trim(tl, loser, inter.start, inter.end, policy,
                       decisions, iteration)
    decisions.append({
        "iteration": iteration,
        "conflict": conflict.to_json(),
        "policy": policy,
        "winner": winner.id,
        "loser": loser.id,
        "action": action,
    })


def _fix_one(tl: Timeline, conflict: Conflict, policy: str,
             decisions: list[dict], iteration: int) -> None:
    found = tl.find_clip(conflict.clips[0])
    if conflict.kind in (SAME_TRACK, CROSS_TRACK):
        _fix_overlap(tl, conflict, policy, decisions, iteration)
        return
    if found is None:
        return
    track, clip = found
    if conflict.kind == ZERO_LENGTH:
        clip.enabled = False
        action = "disable(zero length)"
    elif conflict.kind == NEGATIVE_START:
        new_dur = clip.range.duration + clip.range.start
        if new_dur <= 0:
            clip.enabled = False
            action = "disable(negative start swallows clip)"
        else:
            clip.range = TimeRange(0, new_dur)
            action = f"clamp-start(0, duration {new_dur})"
    elif conflict.kind == TRANSITION_ORPHAN:
        track.items.remove(clip)
        action = "drop-transition(adjacency missing)"
    else:
        return
    decisions.append({
        "iteration": iteration,
        "conflict": conflict.to_json(),
        "policy": policy,
        "winner": None,
        "loser": clip.id,
        "action": action,
    })


def resolve(timeline: Timeline, policy: str = "track-priority",
            max_iter: int = DEFAULT_MAX_ITER) -> ResolveResult:
    if policy not in POLICIES:
        raise ValueError(f"unknown policy {policy!r}; choose from {POLICIES}")
    tl = copy.deepcopy(timeline)
    result = ResolveResult(timeline=tl)
    result.detected = len(detect(tl))
    seen: set[str] = set()
    for iteration in range(max_iter):
        conflicts = detect(tl)
        result.iterations = iteration
        if not conflicts:
            return result
        state = _state_hash(tl)
        if state in seen:
            result.cycle = True
            result.decisions.append({
                "iteration": iteration, "conflict": None, "policy": policy,
                "winner": None, "loser": None,
                "action": "abort(cycle detected)",
            })
            break
        seen.add(state)
        _fix_one(tl, conflicts[0], policy, result.decisions, iteration)
    result.unresolved = len(detect(tl))
    return result
