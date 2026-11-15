"""Sweep-line conflict detection, policy resolution, and duration accounting."""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Any, Iterable

from model import Clip, Timeline, Track, rational_json, timeline_sort_key

KNOWN_POLICIES = {
    "trim-left", "trim-right", "split", "upper-covers", "protect-locked", "reject",
}
CONFLICT_KINDS = ("intra_track", "cross_layer", "locked_intrusion")
DURATION_TOLERANCE_FRAMES = 2


class ResolutionError(ValueError):
    def __init__(self, code: str, message: str, *, exit_code: int = 1, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.exit_code = exit_code
        self.details = details


@dataclass
class ScanResult:
    records: list[dict[str, Any]]
    by_kind: dict[str, int]
    by_track: dict[str, dict[str, int]]
    comparisons: int

    @property
    def conflict_count(self) -> int:
        return sum(self.by_kind.values())

    def summary(self) -> dict[str, Any]:
        totals = {track: sum(bucket.values()) for track, bucket in self.by_track.items()}
        return {
            "conflict_count": self.conflict_count,
            "conflict_pair_count": len(self.records),
            "conflicts_by_kind": dict(self.by_kind),
            "conflicts_by_track": totals,
            "conflicts_by_track_kind": self.by_track,
            "scan_comparisons": self.comparisons,
        }


def _kind_counts(records: Iterable[dict[str, Any]]) -> dict[str, int]:
    counts = {kind: 0 for kind in CONFLICT_KINDS}
    for record in records:
        for kind in record["kinds"]:
            counts[kind] += 1
    return counts


def _track_counts(records: Iterable[dict[str, Any]]) -> dict[str, dict[str, int]]:
    counts: dict[str, dict[str, int]] = {}
    for record in records:
        for track in record["tracks"]:
            bucket = counts.setdefault(track, {kind: 0 for kind in CONFLICT_KINDS})
            for kind in record["kinds"]:
                bucket[kind] += 1
    return counts


def scan_conflicts(timeline: Timeline) -> ScanResult:
    """Detect overlaps with one sorted sweep; each pair is visited only when active."""
    clips = sorted(timeline.content_clips(), key=lambda clip: (clip.start, -clip.layer, clip.id))
    active: list[Clip] = []
    records_by_pair: dict[tuple[str, str], dict[str, Any]] = {}
    comparisons = 0
    for clip in clips:
        active = [item for item in active if item.end > clip.start]
        for other in active:
            comparisons += 1
            if not other.overlaps(clip):
                continue
            pair = tuple(sorted((other.id, clip.id)))
            record = records_by_pair.get(pair)
            kinds: list[str] = []
            if other.track == clip.track:
                kinds.append("intra_track")
            else:
                kinds.append("cross_layer")
            if other.locked or clip.locked:
                kinds.append("locked_intrusion")
            start, end = max(other.start, clip.start), min(other.end, clip.end)
            if record is None:
                record = {
                    "id": f"conflict_{len(records_by_pair):06d}", "clip_a": pair[0], "clip_b": pair[1],
                    "tracks": sorted({other.track, clip.track}), "layers": sorted({other.layer, clip.layer}),
                    "start": start, "end": end, "kinds": kinds,
                    "locked": bool(other.locked or clip.locked),
                }
                records_by_pair[pair] = record
            else:
                record["start"] = min(record["start"], start)
                record["end"] = max(record["end"], end)
                record["kinds"] = sorted(set(record["kinds"]) | set(kinds))
        active.append(clip)
    records = [records_by_pair[key] for key in sorted(records_by_pair)]
    for record in records:
        record["start"] = rational_json(record["start"])
        record["end"] = rational_json(record["end"])
    return ScanResult(records, _kind_counts(records), _track_counts(records), comparisons)


def _track_order(timeline: Timeline) -> dict[str, int]:
    return {track.name: index for index, track in enumerate(sorted(timeline.tracks, key=timeline_sort_key))}


def _choose_winner(first: Clip, second: Clip, policy: str, order: dict[str, int]) -> tuple[Clip, Clip, str]:
    if policy == "protect-locked":
        if first.locked and second.locked:
            raise ResolutionError("locked_segment_move_required", "two locked clips overlap",
                                  clip_ids=[first.id, second.id])
        if first.locked != second.locked:
            winner, loser = (first, second) if first.locked else (second, first)
            return winner, loser, "protect-locked"
    def key(clip: Clip) -> tuple[Any, ...]:
        return (clip.layer, clip.priority, int(clip.locked), -clip.start,
                -order.get(clip.track, 0), clip.id)
    if policy == "upper-covers" and first.layer != second.layer:
        winner, loser = (first, second) if first.layer > second.layer else (second, first)
    else:
        winner, loser = (first, second) if key(first) > key(second) else (second, first)
    return winner, loser, policy


def _clip_after(clip: Clip, *, start: Fraction | None = None,
                duration: Fraction | None = None, new_id: str | None = None,
                action: str) -> Clip:
    return clip.changed(
        id=new_id or clip.id,
        start=clip.start if start is None else start,
        duration=clip.duration if duration is None else duration,
        action=action,
    )


def _subtract_winner(loser: Clip, winner: Clip, policy: str) -> tuple[list[Clip], dict[str, Any]]:
    overlap_start, overlap_end = max(loser.start, winner.start), min(loser.end, winner.end)
    before = {"start": rational_json(loser.start), "duration": rational_json(loser.duration)}
    if policy == "reject":
        fragments: list[Clip] = []
        action = "reject"
    elif policy == "trim-left":
        fragments = ([_clip_after(loser, start=overlap_end, duration=loser.end - overlap_end,
                                  action="trim-left")] if overlap_end < loser.end else [])
        action = "trim-left"
    elif policy == "trim-right":
        fragments = ([_clip_after(loser, duration=overlap_start - loser.start,
                                  action="trim-right")] if overlap_start > loser.start else [])
        action = "trim-right"
    else:
        left = overlap_start > loser.start
        right = overlap_end < loser.end
        fragments = []
        if left:
            fragments.append(_clip_after(loser, duration=overlap_start - loser.start,
                                         new_id=f"{loser.id}#L", action="trim-right"))
        if right:
            fragments.append(_clip_after(loser, start=overlap_end, duration=loser.end - overlap_end,
                                         new_id=f"{loser.id}#R", action="trim-left"))
        if left and right:
            action = "split"
        elif not fragments:
            action = policy if policy in {"protect-locked", "upper-covers", "reject"} else "trim-left"
        elif left:
            action = "trim-right"
        else:
            action = "trim-left"
    after = [{"id": clip.id, "start": rational_json(clip.start),
              "duration": rational_json(clip.duration)} for clip in fragments]
    return fragments, {"action": action, "before": before, "after": after,
                       "winner": winner.id, "loser": loser.id}


def _replace_clip(tracks: list[Track], loser: Clip, fragments: list[Clip]) -> list[Track]:
    rebuilt: list[Track] = []
    for track in tracks:
        if track.name != loser.track:
            rebuilt.append(track)
            continue
        updated: list[Clip] = []
        for clip in track.clips:
            if clip.id == loser.id and clip.start == loser.start and clip.duration == loser.duration:
                updated.extend(fragments)
            else:
                updated.append(clip)
        rebuilt.append(track.with_clips(updated))
    return rebuilt


def resolve_timeline(timeline: Timeline, policy: str | None = None,
                     max_rounds: int = 100) -> tuple[Timeline, ScanResult, list[dict[str, Any]]]:
    policy = str(policy or timeline.policy)
    if policy not in KNOWN_POLICIES:
        raise ResolutionError("unknown_policy", f"unknown policy {policy!r}", exit_code=1)
    current = timeline
    actions: list[dict[str, Any]] = []
    seen: set[tuple[Any, ...]] = set()
    initial_scan = scan_conflicts(current)
    for _round in range(max_rounds):
        scan = scan_conflicts(current)
        if not scan.records:
            current = current.with_tracks((Track(track.name, track.layer, track.priority,
                                                tuple(sorted(track.clips, key=lambda c: (c.start, c.id))),
                                                track.metadata) for track in current.tracks))
            return current, initial_scan, actions
        state = tuple((clip.track, clip.id, clip.start, clip.duration, clip.action)
                      for clip in sorted(current.all_clips(), key=lambda c: (c.track, c.start, c.id)))
        if state in seen:
            raise ResolutionError("cycle", "resolution repeated an interval state", records=len(scan.records))
        seen.add(state)
        order = _track_order(current)
        progressed = False
        for record in scan.records:
            first, second = current.find_clip(record["clip_a"]), current.find_clip(record["clip_b"])
            if first is None or second is None or not first.overlaps(second):
                continue
            winner, loser, action_hint = _choose_winner(first, second, policy, order)
            if loser.locked and policy != "protect-locked":
                raise ResolutionError("locked_segment_move_required",
                                      f"policy {policy} would move locked clip {loser.id}",
                                      clip_ids=[loser.id])
            fragments, detail = _subtract_winner(loser, winner, policy)
            detail.update({"track": loser.track, "winning_track": winner.track,
                           "action_hint": action_hint, "overlap_start": rational_json(max(first.start, second.start)),
                           "overlap_end": rational_json(min(first.end, second.end))})
            current = current.with_tracks(_replace_clip(list(current.tracks), loser, fragments))
            actions.append(detail)
            progressed = True
        if not progressed:
            raise ResolutionError("cycle", "conflicts remained but no interval could progress")
    raise ResolutionError("cycle", "resolution did not reach a fixed point", rounds=max_rounds)


def _merged_ranges(timeline: Timeline, *, clip_start: Fraction | None = None,
                   clip_end: Fraction | None = None) -> list[tuple[Fraction, Fraction]]:
    ranges: list[tuple[Fraction, Fraction]] = []
    for clip in sorted(timeline.content_clips(), key=lambda item: item.start):
        start = clip.start if clip_start is None else max(clip_start, clip.start)
        end = clip.end if clip_end is None else min(clip_end, clip.end)
        if end <= start:
            continue
        if ranges and start <= ranges[-1][1]:
            ranges[-1] = (ranges[-1][0], max(ranges[-1][1], end))
        else:
            ranges.append((start, end))
    return ranges


def duration_metrics(timeline: Timeline) -> dict[str, Any]:
    master = timeline.master_frames
    ranges = _merged_ranges(timeline)
    master_ranges = _merged_ranges(timeline, clip_start=Fraction(0), clip_end=Fraction(master))
    coverage_measure = sum((end - start for start, end in ranges), Fraction(0))
    master_coverage = sum((end - start for start, end in master_ranges), Fraction(0))
    resolved_frames = sum((end.__ceil__() - start.__floor__() for start, end in ranges), 0)
    master_covered_frames = sum((end.__ceil__() - start.__ceil__() for start, end in master_ranges), 0)
    negative_trimmed = sum(
        (min(clip.end, Fraction(0)) - clip.start for clip in timeline.content_clips() if clip.start < 0),
        Fraction(0),
    )
    tail_trimmed = sum(
        (clip.end - max(clip.start, Fraction(master)) for clip in timeline.content_clips() if clip.end > master),
        Fraction(0),
    )
    delta = resolved_frames - master
    rate = timeline.rate
    delta_seconds = Fraction(delta, 1) / rate
    total_seconds = Fraction(resolved_frames, 1) / rate
    return {
        "resolved_frames": resolved_frames,
        "resolved_frames_exact": rational_json(coverage_measure),
        "master_covered_frames": master_covered_frames,
        "master_covered_frames_exact": rational_json(master_coverage),
        "master_frames": master,
        "duration_delta_frames": delta,
        "duration_delta_seconds": round(float(delta_seconds), 9),
        "total_duration_seconds": round(float(total_seconds), 9),
        "duration_delta_exact_seconds": rational_json(delta_seconds),
        "negative_preroll_trimmed": rational_json(negative_trimmed),
        "tail_trimmed": rational_json(tail_trimmed),
        "subframe_compensation": rational_json(Fraction(resolved_frames) - coverage_measure),
        "over_limit": abs(delta) > DURATION_TOLERANCE_FRAMES,
        "duration_reason_code": "duration_limit_exceeded" if abs(delta) > DURATION_TOLERANCE_FRAMES else None,
    }


def _interval_payload(clip: Clip) -> dict[str, Any]:
    payload = {
        "id": clip.id, "track": clip.track, "start": rational_json(clip.start),
        "duration": rational_json(clip.duration), "src": clip.src, "layer": clip.layer,
        "priority": clip.priority, "locked": clip.locked, "action": clip.action,
        "kind": clip.kind,
    }
    if clip.policy is not None:
        payload["policy"] = clip.policy
    if clip.source_start is not None:
        payload["source_start"] = rational_json(clip.source_start)
    if clip.source_rate is not None:
        payload["source_rate"] = rational_json(clip.source_rate)
    return payload


def build_resolution_report(timeline: Timeline, resolved: Timeline, scan: ScanResult,
                            actions: list[dict[str, Any]], *, policy: str,
                            cross_format_hash_converged: bool = True,
                            cross_format_checked: bool = False,
                            format_frame_sha256: dict[str, str] | None = None,
                            precision_losses: list[dict[str, Any]] | None = None) -> dict[str, Any]:
    metrics = duration_metrics(resolved)
    intervals = sorted(
        (_interval_payload(clip) for clip in resolved.content_clips()),
        key=lambda item: (item["track"], Fraction(item["start"]["num"], item["start"]["den"]), item["id"]),
    )
    gaps = sorted(
        (_interval_payload(clip) for track in resolved.tracks for clip in track.clips if clip.kind == "gap"),
        key=lambda item: (item["track"], Fraction(item["start"]["num"], item["start"]["den"]), item["id"]),
    )
    exit_code = 3 if metrics["over_limit"] or not cross_format_hash_converged else 0
    quarantine: list[dict[str, Any]] = []
    if metrics["over_limit"]:
        quarantine.append({"code": "duration_limit_exceeded", "scope": "project",
                           "reason": "duration_limit_exceeded"})
    if not cross_format_hash_converged:
        quarantine.append({"code": "cross_format_hash_not_converged", "scope": "project",
                           "reason": "cross_format_hash_not_converged"})
    report = {
        "status": "resolved" if exit_code == 0 else "deviation",
        "project": resolved.name,
        "source_format": timeline.source_format,
        "policy": policy,
        "rate": rational_json(resolved.rate),
        "master_frames": resolved.master_frames,
        "conflict_count": scan.conflict_count,
        "conflict_pair_count": len(scan.records),
        "conflicts_by_kind": dict(scan.by_kind),
        "conflicts_by_track": {track: sum(bucket.values()) for track, bucket in scan.by_track.items()},
        "conflicts_by_track_kind": scan.by_track,
        "scan_comparisons": scan.comparisons,
        "conflicts": scan.records,
        "resolved_intervals": intervals,
        "resolved_interval_count": len(intervals),
        "gaps": gaps,
        "gap_count": len(gaps),
        "track_specs": [
            {"name": track.name, "layer": track.layer, "priority": track.priority,
             "clip_count": len(track.clips)}
            for track in sorted(resolved.tracks, key=timeline_sort_key)
        ],
        "actions": actions,
        "action_count": len(actions),
        "cross_format_hash_converged": cross_format_hash_converged,
        "cross_format_checked": cross_format_checked,
        "format_frame_sha256": format_frame_sha256 or {},
        "precision_losses": (precision_losses if precision_losses is not None
                             else [dict(item) for item in timeline.precision_losses]),
        "quarantine": quarantine,
        "quarantine_reasons": {item["code"]: 1 for item in quarantine},
        "exit_code": exit_code,
        **metrics,
    }
    return report
