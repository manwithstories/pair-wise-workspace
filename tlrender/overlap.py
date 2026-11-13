"""Same-track overlap resolution on half-open intervals [start, end).

Touching endpoints (a.end == b.start) do not overlap. Cues are processed in
the stable order (track, start, priority desc, id). A higher-priority cue is
always kept intact; the lower-priority side is truncated. A remainder shorter
than MIN_READABLE_MS (400 ms) is marked dropped. Every truncation increments
that cue's ``resolved`` counter.
"""
from __future__ import annotations

from . import MIN_READABLE_MS
from .tl_parse import Cue


def _truncate(victim: Cue, anchor_start: int, anchor_end: int) -> None:
    start, end = victim.final_start, victim.final_end
    left = anchor_start - start
    right = end - anchor_end
    if left >= right and left > 0:
        victim.final_end = anchor_start
    elif right > 0:
        victim.final_start = anchor_end
    else:
        victim.final_end = victim.final_start
    victim.resolved += 1


def resolve_overlaps(cues: list[Cue]) -> list[Cue]:
    ordered = sorted(cues, key=lambda c: (c.track, c.start, -c.priority, c.id))
    kept: list[Cue] = []
    for cue in ordered:
        if cue.final_start is None:
            cue.final_start, cue.final_end = cue.start, cue.end
        for anchor in kept:
            if anchor.track != cue.track or anchor.dropped:
                continue
            if cue.final_start >= cue.final_end:
                break
            a_start, a_end = anchor.final_start, anchor.final_end
            c_start, c_end = cue.final_start, cue.final_end
            if c_start >= a_end or c_end <= a_start:
                continue
            if cue.priority > anchor.priority:
                _truncate(anchor, c_start, c_end)
                if anchor.final_end - anchor.final_start < MIN_READABLE_MS:
                    anchor.dropped = True
                continue
            _truncate(cue, a_start, a_end)
        if (
            not cue.dropped
            and cue.final_end - cue.final_start < MIN_READABLE_MS
        ):
            cue.dropped = True
        kept.append(cue)
    return kept


def count_resolved(cues: list[Cue]) -> int:
    return sum(1 for cue in cues if cue.resolved > 0)


def count_dropped(cues: list[Cue]) -> int:
    return sum(1 for cue in cues if cue.dropped)
