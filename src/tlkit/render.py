"""Frame-plan rendering. Consumes the model only; never parses files.

For every frame in [0, duration) the visible owner is the topmost video
track (lowest index) with an enabled, unmuted clip covering that frame.
Two clips covering the same frame on one track raise RenderError
("same frame, multiple masters").
"""
from __future__ import annotations

import hashlib
import json

from .model import VIDEO, Clip, Timeline


class RenderError(Exception):
    pass


def _track_runs(track_name: str, clips: list[Clip], duration: int) -> list[tuple[int, int, str | None]]:
    ordered = sorted(clips, key=lambda c: (c.range.start, c.id))
    runs: list[tuple[int, int, str | None]] = []
    cursor = 0
    prev: Clip | None = None
    for clip in ordered:
        if prev is not None and clip.range.start < prev.range.end:
            raise RenderError(
                f"track {track_name}: frame {clip.range.start} has multiple "
                f"masters ({prev.id}, {clip.id})")
        prev = clip
        start = max(clip.range.start, 0)
        end = min(clip.range.end, duration)
        if end <= start:
            continue
        if cursor < start:
            runs.append((cursor, start, None))
        runs.append((start, end, clip.id))
        cursor = end
    if cursor < duration:
        runs.append((cursor, duration, None))
    return runs


def frame_plan(timeline: Timeline) -> list[dict]:
    """Compact runs: consecutive frames with identical layer ownership."""
    duration = timeline.duration
    video_tracks = [t for t in timeline.tracks if t.kind == VIDEO]
    per_track: dict[str, list[tuple[int, int, str | None]]] = {}
    for tr in video_tracks:
        visible = [c for c in tr.clips() if c.visible]
        per_track[tr.name] = _track_runs(tr.name, visible, duration)

    boundaries = {0, duration}
    for runs in per_track.values():
        for start, end, _ in runs:
            boundaries.add(start)
            boundaries.add(end)
    points = sorted(boundaries)

    def owner_at(track: str, frame: int) -> str | None:
        for start, end, cid in per_track[track]:
            if start <= frame < end:
                return cid
        return None

    plan: list[dict] = []
    names = [t.name for t in video_tracks]
    for lo, hi in zip(points, points[1:]):
        if lo >= hi:
            continue
        layers = {n: owner_at(n, lo) for n in names}
        layers = {n: c for n, c in layers.items() if c is not None}
        owner = layers[names[0]] if names and names[0] in layers else None
        for n in names:  # topmost track with a clip wins the frame
            if n in layers:
                owner = layers[n]
                break
        plan.append({"start": lo, "end": hi, "owner": owner, "layers": layers})
    return plan


def plan_hash(timeline: Timeline, plan: list[dict] | None = None) -> str:
    if plan is None:
        plan = frame_plan(timeline)
    blob = json.dumps({
        "fps": f"{timeline.fps.numerator}/{timeline.fps.denominator}",
        "duration": timeline.duration,
        "runs": plan,
    }, sort_keys=True).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()
