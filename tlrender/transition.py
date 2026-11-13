"""Transition windows and subtitle avoidance.

Transitions are half-open windows [start, end); a ``cut`` is a zero-width
point. A cue may not straddle a transition boundary. When trimming would push
the cue below the minimum readable duration (400 ms) the whole cue is
translated instead, choosing the shorter move.
"""
from __future__ import annotations

from . import MIN_READABLE_MS
from .tl_parse import Cue, Transition


def sort_transitions(transitions: list[Transition]) -> list[Transition]:
    return sorted(transitions, key=lambda t: (t.start, t.end, t.kind))


def _fits(cue: Cue, start: int, end: int, windows) -> bool:
    if end - start < MIN_READABLE_MS or start < 0:
        return False
    for win in windows:
        if start < win.end and win.start < end:
            return False
    return True


def apply_transitions(
    cues: list[Cue], transitions: list[Transition], max_passes: int = 8
) -> list[Cue]:
    windows = sort_transitions(transitions)
    for cue in cues:
        if cue.final_start is None:
            cue.final_start, cue.final_end = cue.start, cue.end
        if cue.dropped:
            continue
        passes = 0
        for win in windows:
            while passes < max_passes:
                start, end = cue.final_start, cue.final_end
                if not (start < win.end and win.start < end):
                    break
                left_trim = win.start - start
                right_trim = end - win.end
                can_left = left_trim >= MIN_READABLE_MS
                can_right = right_trim >= MIN_READABLE_MS
                if can_left and (not can_right or left_trim >= right_trim):
                    cue.final_end = win.start
                    cue.moved += 1
                    break
                if can_right:
                    cue.final_start = win.end
                    cue.moved += 1
                    break
                duration = end - start
                left_shift = max(0, end - win.start)
                left_candidate = max(0, win.start - left_shift)
                right_candidate = win.end
                left_move = start - left_candidate
                right_move = right_candidate - start
                if left_move <= right_move and _fits(
                    cue, left_candidate, left_candidate + duration, windows
                ):
                    cue.final_start = left_candidate
                    cue.final_end = left_candidate + duration
                elif _fits(cue, right_candidate, right_candidate + duration, windows):
                    cue.final_start = right_candidate
                    cue.final_end = right_candidate + duration
                else:
                    cue.final_start = max(0, left_candidate)
                    cue.final_end = max(MIN_READABLE_MS, cue.final_start + duration)
                cue.moved += 1
                passes += 1
    return cues
