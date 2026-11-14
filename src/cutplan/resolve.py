from __future__ import annotations

from bisect import bisect_right
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any

from .errors import CutplanError
from .interval import merge_intervals, overlaps
from .model import Clip, Timeline, Track
from .timebase import FrameInterval
from .validate import (
    beat_bpm,
    closeup_violations,
    is_beat,
    is_closeup,
    is_main,
    nearest_beat,
    validate_timeline,
)


POLICIES = ("trim_head", "trim_tail", "ripple", "reject")


@dataclass(slots=True)
class Resolution:
    timeline: Timeline
    stats: "ResolutionStats"
    report: dict[str, Any]


@dataclass(slots=True)
class ResolutionStats:
    policy: str = ""
    removed_zero: int = 0
    removed_rejected: int = 0
    ripple_shifted: int = 0
    orphaned: int = 0
    audio_start_rounded: int = 0
    shifted_ids: set[str] = field(default_factory=set)
    orphans: list[dict[str, Any]] = field(default_factory=list)
    conflicts_before: dict[str, Any] = field(default_factory=dict)


def resolve_timeline(
    timeline: Timeline,
    policy: str = "trim_head",
    *,
    cross_track: str | None = None,
) -> Resolution:
    if policy not in POLICIES:
        raise CutplanError("E_POLICY_INVALID", f"unknown policy {policy!r}; use {', '.join(POLICIES)}")
    working = timeline.clone()
    stats = ResolutionStats()
    stats.policy = policy
    working.metadata["resolution_policy"] = policy
    stats.audio_start_rounded = int(
        working.metadata.pop("_audio_start_rounded", working.metadata.pop("audio_start_rounded", 0)) or 0
    )
    stats.conflicts_before = validate_timeline(working).conflicts
    _remove_zeros(working, stats)
    _resolve_conflicts(working, policy, stats)
    action = cross_track or _cross_track_action(working)
    if action not in {"truncate", "orphan"}:
        raise CutplanError("E_CROSS_TRACK_INVALID", f"unknown cross-track action {action!r}")
    _apply_cross_track(working, action, stats)
    _remove_zeros(working, stats)
    for _ in range(4):
        _resolve_conflicts(working, policy, stats)
        _apply_cross_track(working, action, stats)
        _remove_zeros(working, stats)
        if validate_timeline(working, include_overlap_errors=False).conflicts["clusters"] == 0:
            break
    report = build_report(working, stats)
    return Resolution(timeline=working, stats=stats, report=report)


resolve = resolve_timeline


def _remove_zeros(timeline: Timeline, stats: ResolutionStats) -> None:
    for track in timeline.tracks:
        kept: list[Clip] = []
        for clip in track.clips:
            if clip.duration <= 0:
                stats.removed_zero += 1
                stats.shifted_ids.discard(clip.id)
            else:
                kept.append(clip)
        track.clips = kept


def _resolve_conflicts(timeline: Timeline, policy: str, stats: ResolutionStats) -> None:
    for track in timeline.tracks:
        if policy == "ripple":
            _ripple_track(track, stats)
        elif policy == "trim_tail":
            _trim_tail_track(track, stats)
        else:
            _trim_track(track, policy, stats)


def _accepted_overlaps(accepted: list[Clip], starts: list[int], candidate: Clip) -> list[Clip]:
    if not accepted:
        return []
    index = bisect_right(starts, candidate.start)
    found: list[Clip] = []
    if index and accepted[index - 1].end > candidate.start:
        found.append(accepted[index - 1])
    while index < len(accepted) and accepted[index].start < candidate.end:
        if accepted[index].end > candidate.start:
            found.append(accepted[index])
        index += 1
    return found


def _insert_accepted(accepted: list[Clip], starts: list[int], clip: Clip) -> None:
    index = bisect_right(starts, clip.start)
    accepted.insert(index, clip)
    starts.insert(index, clip.start)


def _trim_track(track: Track, policy: str, stats: ResolutionStats) -> None:
    accepted: list[Clip] = []
    starts: list[int] = []
    if policy == "reject":
        ordered = sorted(track.clips, key=lambda item: item.order_key())
    else:
        ordered = sorted(track.clips, key=lambda item: (item.start, -item.priority, item.id))
    for original in ordered:
        clip = original.clone()
        collisions = _accepted_overlaps(accepted, starts, clip)
        if not collisions:
            _insert_accepted(accepted, starts, clip)
            continue
        if policy == "reject":
            stats.removed_rejected += 1
            continue
        if policy == "trim_head":
            old_start = clip.start
            old_end = clip.end
            clip.start = max(item.end for item in collisions)
            clip.duration = old_end - clip.start if old_end > clip.start else 0
            if clip.start != old_start and clip.duration > 0:
                clip.source_start += clip.start - old_start
                _mark_shifted(stats, clip)
        if clip.duration <= 0:
            stats.removed_zero += 1
            stats.shifted_ids.discard(clip.id)
            continue
        _insert_accepted(accepted, starts, clip)
    track.clips = sorted(accepted, key=lambda item: (item.start, item.id))


def _trim_tail_track(track: Track, stats: ResolutionStats) -> None:
    guard = 0
    while True:
        ordered = sorted(track.clips, key=lambda item: (item.start, -item.priority, item.id))
        pair = _next_overlap(ordered)
        if pair is None:
            break
        guard += 1
        if guard > len(ordered) * 4 + 8:
            raise CutplanError("E_CASCADE_LIMIT", "trim_tail did not converge", exit_code=4)
        first, second = sorted(pair, key=lambda item: (item.start, item.priority, item.id))
        if first.start < second.start:
            target, boundary = first, second.start
        else:
            target = max(pair, key=lambda item: item.order_key())
            boundary = min(item.start for item in pair if item is not target)
        target.duration = max(0, boundary - target.start)
        if target.duration == 0:
            stats.removed_zero += 1
            stats.shifted_ids.discard(target.id)
            track.clips = [clip for clip in track.clips if clip.id != target.id]


def _ripple_track(track: Track, stats: ResolutionStats) -> None:
    guard = 0
    while True:
        ordered = sorted(track.clips, key=lambda item: (item.start, item.id))
        pair = _next_overlap(ordered)
        if pair is None:
            break
        guard += 1
        if guard > len(ordered) * 4 + 8:
            raise CutplanError("E_CASCADE_LIMIT", "ripple did not converge", exit_code=4)
        first, second = pair
        winner, loser = sorted((first, second), key=lambda item: item.order_key())
        delta = winner.end - loser.start
        if delta <= 0:
            raise CutplanError("E_RESOLVE_STATE", "invalid ripple overlap", exit_code=4)
        old_start = loser.start
        for item in track.clips:
            if item.id == winner.id:
                continue
            if item.start >= old_start:
                item.start += delta
                _mark_shifted(stats, item)
        track.clips.sort(key=lambda item: (item.start, item.id))
    kept = [clip for clip in track.clips if clip.duration > 0]
    stats.removed_zero += len(track.clips) - len(kept)
    track.clips = kept


def _next_overlap(ordered: list[Clip]) -> tuple[Clip, Clip] | None:
    for index, first in enumerate(ordered):
        for second in ordered[index + 1 :]:
            if second.start >= first.end:
                break
            if overlaps(first.interval, second.interval):
                return first, second
    return None


def _mark_shifted(stats: ResolutionStats, clip: Clip) -> None:
    stats.shifted_ids.add(clip.id)


def _cross_track_action(timeline: Timeline) -> str:
    for source in (timeline.ext, timeline.manifest.ext):
        value = source.get("cross_track_action")
        if isinstance(value, str):
            return value
        nested = source.get("cross_track")
        if isinstance(nested, dict) and isinstance(nested.get("action"), str):
            return nested["action"]
    return "truncate"


def _apply_cross_track(timeline: Timeline, action: str, stats: ResolutionStats) -> None:
    main = merge_intervals(
        [clip.interval for track in timeline.tracks if is_main(track) for clip in track.clips]
    )
    for track in timeline.tracks:
        if is_closeup(track):
            _fit_closeups(track, main, action, stats)
        elif is_beat(track):
            _snap_beats(track, timeline, action, stats)


def _fit_closeups(
    track: Track, main: list[FrameInterval], action: str, stats: ResolutionStats
) -> None:
    kept: list[Clip] = []
    for clip in track.clips:
        if any(interval.start <= clip.start and clip.end <= interval.end for interval in main):
            kept.append(clip)
            continue
        if action == "orphan":
            _orphan(track, clip, "closeup_outside_main", stats)
            continue
        best = None
        for interval in main:
            start, end = max(clip.start, interval.start), min(clip.end, interval.end)
            if start < end and (best is None or end - start > best.end - best.start):
                best = FrameInterval(start, end)
        if best is None:
            _orphan(track, clip, "closeup_outside_main", stats)
            continue
        if best.start != clip.start:
            clip.source_start += best.start - clip.start
        clip.start, clip.duration = best.start, best.duration
        _mark_shifted(stats, clip)
        kept.append(clip)
    track.clips = sorted(kept, key=lambda item: (item.start, item.id))


def _snap_beats(track: Track, timeline: Timeline, action: str, stats: ResolutionStats) -> None:
    kept: list[Clip] = []
    for clip in track.clips:
        nearest, distance = nearest_beat(clip.start, timeline)
        if distance <= 2:
            if nearest != clip.start:
                clip.start = nearest
                _mark_shifted(stats, clip)
            kept.append(clip)
        else:
            _orphan(track, clip, "beat_off_grid", stats)
    track.clips = sorted(kept, key=lambda item: (item.start, item.id))


def _orphan(track: Track, clip: Clip, reason: str, stats: ResolutionStats) -> None:
    stats.orphaned += 1
    stats.shifted_ids.discard(clip.id)
    stats.orphans.append({"track_id": track.id, "clip_id": clip.id, "reason": reason})


def build_report(timeline: Timeline, stats: ResolutionStats) -> dict[str, Any]:
    validation = validate_timeline(timeline, include_overlap_errors=False)
    audio_rounded = stats.audio_start_rounded or validation.audio_start_rounded
    intervals_by_track: list[dict[str, Any]] = []
    track_details: list[dict[str, Any]] = []
    for track in sorted(timeline.tracks, key=lambda item: item.id):
        merged = merge_intervals(track.intervals)
        intervals = [{"start": item.start, "end": item.end} for item in merged]
        coverage = sum(item.duration for item in merged)
        intervals_by_track.extend(
            {"track_id": track.id, "start": item.start, "end": item.end} for item in merged
        )
        track_details.append(
            {
                "id": track.id,
                "kind": track.kind,
                "clip_count": len(track.clips),
                "coverage_frames": coverage,
                "resolved_intervals": intervals,
            }
        )
    delta_frames = timeline.duration_frames - timeline.declared_duration
    live_ids = {clip.id for track in timeline.tracks for clip in track.clips}
    shifted = len(stats.shifted_ids & live_ids)
    conflicts_after = int(validation.conflicts["clusters"])
    return {
        "ok": conflicts_after == 0 and validation.ok,
        "policy": stats.policy,
        "resolved_clips": timeline.clip_count,
        "removed_zero": stats.removed_zero,
        "removed_rejected": stats.removed_rejected,
        "ripple_shifted": shifted,
        "orphaned": stats.orphaned,
        "orphans": stats.orphans,
        "audio_start_rounded": audio_rounded,
        "clusters_before": stats.conflicts_before.get("clusters", 0),
        "pairs_before": stats.conflicts_before.get("pairs", 0),
        "clusters_after": conflicts_after,
        "duration_frames": timeline.duration_frames,
        "duration_seconds": _seconds(timeline.duration_frames, timeline.rate.fps),
        "output_duration_frames": timeline.output_duration_frames,
        "declared_duration_frames": timeline.declared_duration,
        "declared_duration_seconds": _seconds(timeline.declared_duration, timeline.rate.fps),
        "duration_delta_frames": delta_frames,
        "duration_delta_seconds": _seconds(delta_frames, timeline.rate.fps),
        "tracks": track_details,
        "resolved_intervals": intervals_by_track,
        "validation_errors": [error.as_dict() for error in validation.errors],
    }

def _seconds(frames: int, fps: Fraction) -> float:
    return float(round(Fraction(frames, 1) / fps, 9))
