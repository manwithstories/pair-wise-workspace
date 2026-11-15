"""Deterministic offscreen frame scheduler and hashing over the model only."""

from __future__ import annotations

import hashlib
from typing import Any

from model import Clip, Timeline, timeline_sort_key


def _frame_bounds(clip: Clip, master_frames: int) -> tuple[int, int]:
    first = max(0, clip.start.__ceil__())
    last = min(master_frames, clip.end.__ceil__())
    return first, last


def _token(clip: Clip) -> bytes:
    text = f"{clip.layer}\x1f{clip.priority}\x1f{clip.track}\x1f{clip.src}\x1f{int(clip.locked)}"
    return text.encode("utf-8")


def render_timeline(timeline: Timeline, master_frames: int | None = None) -> dict[str, Any]:
    """Render ownership per integer frame; no exchange document is parsed here."""
    total = timeline.master_frames if master_frames is None else int(master_frames)
    if total < 0:
        raise ValueError("master_frames must be non-negative")
    order = {track.name: index for index, track in enumerate(sorted(timeline.tracks, key=timeline_sort_key))}
    owners: list[Clip | None] = [None] * total
    for clip in timeline.content_clips():
        first, last = _frame_bounds(clip, total)
        if last <= first:
            continue
        key = clip.ownership_key(order.get(clip.track, 0))
        for frame in range(first, last):
            current = owners[frame]
            if current is None or key > current.ownership_key(order.get(current.track, 0)):
                owners[frame] = clip

    canonical = hashlib.sha256()
    track_hashes = {name: hashlib.sha256() for name in timeline.track_names()}
    source_counts: dict[str, int] = {}
    track_counts = {name: 0 for name in timeline.track_names()}
    layer_counts: dict[str, int] = {}
    runs: list[dict[str, Any]] = []
    uncovered = 0
    run_start = 0
    previous: Clip | None = None
    for frame, owner in enumerate(owners):
        if owner is None:
            canonical.update(b"\x00")
            uncovered += 1
            for digest in track_hashes.values():
                digest.update(b"\x00")
        else:
            canonical.update(_token(owner) + b"\n")
            source_counts[owner.src] = source_counts.get(owner.src, 0) + 1
            track_counts[owner.track] = track_counts.get(owner.track, 0) + 1
            layer_key = str(owner.layer)
            layer_counts[layer_key] = layer_counts.get(layer_key, 0) + 1
            for name, digest in track_hashes.items():
                digest.update((_token(owner) + b"\n") if owner.track == name else b"\x00")
        if owner is not previous:
            if previous is not None:
                runs.append({
                    "start": run_start, "duration": frame - run_start,
                    "track": previous.track, "src": previous.src, "layer": previous.layer,
                    "clip_id": previous.id, "locked": previous.locked,
                })
            run_start, previous = frame, owner
    if previous is not None:
        runs.append({
            "start": run_start, "duration": total - run_start, "track": previous.track,
            "src": previous.src, "layer": previous.layer, "clip_id": previous.id,
            "locked": previous.locked,
        })
    source_coverage = {
        src: {"frames": count, "ratio": round(count / total, 9) if total else 0.0}
        for src, count in sorted(source_counts.items())
    }
    return {
        "frames_hashed": total,
        "uncovered_frames": uncovered,
        "canonical_frame_sha256": canonical.hexdigest(),
        "per_track_frame_sha256": {name: digest.hexdigest() for name, digest in sorted(track_hashes.items())},
        "source_coverage": source_coverage,
        "track_covered_frames": track_counts,
        "top_layer_histogram": dict(sorted(layer_counts.items(), key=lambda item: int(item[0]))),
        "frame_ownership_runs": runs,
    }


def hashes_match(first: dict[str, Any], second: dict[str, Any]) -> bool:
    return (first.get("canonical_frame_sha256") == second.get("canonical_frame_sha256") and
            first.get("per_track_frame_sha256") == second.get("per_track_frame_sha256"))
