"""Conflict resolution policies over overlap clusters."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable

from .conflict import Conflict, scan_conflicts
from .model import Segment, Timeline, Track

POLICIES = ("trim", "ripple", "spill", "drop", "fill")


class ResolveError(ValueError):
    pass


@dataclass
class ResolutionResult:
    timeline: Timeline
    before_samples: int
    after_samples: int
    unresolved: list[Conflict] = field(default_factory=list)
    applied: dict[str, int] = field(default_factory=dict)
    fillers_added: int = 0

    @property
    def resolved_intervals(self) -> int:
        return sum(
            1
            for _, segment in self.timeline.iter_segments()
            if segment.duration > 0
        )

    @property
    def drift_samples(self) -> int:
        return abs(self.after_samples - self.before_samples)

    def as_dict(self) -> dict[str, Any]:
        return {
            "resolved_intervals": self.resolved_intervals,
            "unresolved_count": len(self.unresolved),
            "unresolved": [item.as_dict() for item in self.unresolved],
            "total_duration_before_samples": self.before_samples,
            "total_duration_after_samples": self.after_samples,
            "drift_samples": self.drift_samples,
            "applied": dict(sorted(self.applied.items())),
            "fillers_added": self.fillers_added,
        }


def _span(timeline: Timeline) -> int:
    return max(timeline.computed_duration(), timeline.declared_duration or 0)


def _overlap_pair(first: Segment, second: Segment) -> int:
    return max(0, min(first.end, second.end) - max(first.start, second.start))


def _trim(segments: list[Segment]) -> tuple[list[Segment], bool]:
    ordered = sorted(segments, key=lambda item: (item.start, item.id))
    kept: list[Segment] = []
    changed = False
    for segment in ordered:
        if kept and segment.start < kept[-1].end:
            cut = kept[-1].end - segment.start
            if cut >= segment.duration:
                changed = True
                continue
            segment.source_start += cut
            segment.start += cut
            segment.duration -= cut
            changed = True
        kept.append(segment)
    return kept, changed


def _ripple(segments: list[Segment], track: Track) -> tuple[list[Segment], bool]:
    ordered = sorted(segments, key=lambda item: (item.start, item.id))
    changed = False
    if not ordered:
        return ordered, changed
    cursor = ordered[0].end
    for segment in ordered[1:]:
        if segment.start < cursor:
            segment.start = cursor
            changed = True
        cursor = max(cursor, segment.end)
    return ordered, changed


def _spill(
    segments: list[Segment], track: Track
) -> tuple[list[Segment], list[tuple[Track, Segment]], bool]:
    ordered = sorted(segments, key=lambda item: (item.start, item.id))
    kept: list[Segment] = []
    spilled: list[tuple[Track, Segment]] = []
    changed = False
    for segment in ordered:
        if kept and segment.start < kept[-1].end:
            new_track = Track(
                id=f"{segment.id}.spill",
                kind=track.kind,
                z_order=track.z_order,
                priority=track.priority,
                metadata={"spilled_from": track.id, "spill": True},
                segments=[segment],
                extensions=dict(track.extensions),
            )
            spilled.append((new_track, segment))
            changed = True
        else:
            kept.append(segment)
    return kept, spilled, changed


def _drop(segments: list[Segment]) -> tuple[list[Segment], bool]:
    current = sorted(segments, key=lambda item: (item.start, item.id))
    changed = False
    while True:
        pair = next(
            (
                (current[index], current[index + 1])
                for index in range(len(current) - 1)
                if _overlap_pair(current[index], current[index + 1]) > 0
            ),
            None,
        )
        if pair is None:
            return current, changed
        first, second = pair
        victim = min(
            (first, second), key=lambda item: (item.priority, -item.start, item.id)
        )
        current.remove(victim)
        changed = True


def _fill_gaps(track: Track, counter: list[int]) -> int:
    ordered = sorted(
        (segment for segment in track.segments if segment.duration > 0),
        key=lambda item: (item.start, item.id),
    )
    additions: list[Segment] = []
    for earlier, later in zip(ordered, ordered[1:]):
        if earlier.end < later.start:
            counter[0] += 1
            additions.append(
                Segment(
                    id=f"fill.{track.id}.{counter[0]}",
                    start=earlier.end,
                    duration=later.start - earlier.end,
                    priority=-1,
                    metadata={"filler": True},
                )
            )
    track.segments.extend(additions)
    track.segments.sort(key=lambda item: (item.start, item.id))
    return len(additions)


def _resolve_cluster(
    timeline: Timeline,
    track: Track,
    cluster: list[Segment],
    policies: Iterable[str],
) -> tuple[list[tuple[Track, Segment]], bool, list[str]]:
    policy_list = list(policies)
    spilled: list[tuple[Track, Segment]] = []
    used: list[str] = []
    current = list(cluster)
    for policy in policy_list:
        if not any(
            _overlap_pair(current[i], current[j]) > 0
            for i in range(len(current))
            for j in range(i + 1, len(current))
        ):
            break
        if policy == "trim":
            current, changed = _trim(current)
        elif policy == "ripple":
            current, changed = _ripple(current, track)
        elif policy == "spill":
            current, additions, changed = _spill(current, track)
            spilled.extend(additions)
        elif policy == "drop":
            current, changed = _drop(current)
        elif policy == "fill":
            current, changed = _trim(current)
        else:
            raise ResolveError(f"unknown policy: {policy}")
        if changed:
            used.append(policy)
    removed = {segment.id for segment in cluster} - {segment.id for segment in current}
    track.segments[:] = [
        segment for segment in track.segments if segment.id not in removed
    ]
    remaining = any(
        _overlap_pair(current[i], current[j]) > 0
        for i in range(len(current))
        for j in range(i + 1, len(current))
    )
    return spilled, remaining, used


def resolve(
    timeline: Timeline,
    policies: Iterable[str] = ("ripple",),
    *,
    scope: str = "cluster",
) -> ResolutionResult:
    policy_list = list(policies)
    if not policy_list:
        policy_list = ["ripple"]
    unknown = [policy for policy in policy_list if policy not in POLICIES]
    if unknown:
        raise ResolveError(f"unknown policy: {', '.join(unknown)}")
    if scope not in ("cluster", "track"):
        raise ResolveError(f"unknown scope: {scope}")
    before = _span(timeline)
    result = timeline.copy()
    unresolved: list[Conflict] = []
    applied = {policy: 0 for policy in policy_list}
    spilled_tracks: list[Track] = []
    fill_counter = [0]
    fillers = 0
    for track in result.tracks:
        if track.kind == "audio":
            continue
        positive = [segment for segment in track.segments if segment.duration > 0]
        if scope == "track":
            clusters = [positive] if positive else []
        else:
            clusters = _clusters(positive)
        for cluster in clusters:
            overlap_exists = any(
                _overlap_pair(cluster[i], cluster[j]) > 0
                for i in range(len(cluster))
                for j in range(i + 1, len(cluster))
            )
            if not overlap_exists and "fill" not in policy_list:
                continue
            locked = [segment for segment in cluster if result.effective_locked(track, segment)]
            if overlap_exists and locked:
                for index, first in enumerate(cluster):
                    for second in cluster[index + 1 :]:
                        amount = _overlap_pair(first, second)
                        if amount > 0:
                            unresolved.append(
                                Conflict(
                                    "unresolvable",
                                    track.id,
                                    (first.id, second.id),
                                    max(first.start, second.start),
                                    min(first.end, second.end),
                                    amount,
                                    True,
                                    "cluster contains locked segment",
                                )
                            )
                continue
            additions, remaining, used = _resolve_cluster(result, track, cluster, policy_list)
            spilled_tracks.extend(track for track, _ in additions)
            for policy in used:
                applied[policy] += 1
            if remaining:
                unresolved.append(
                    Conflict(
                        "unresolvable",
                        track.id,
                        tuple(segment.id for segment in cluster),
                        min(segment.start for segment in cluster),
                        max(segment.end for segment in cluster),
                        0,
                        True,
                        f"policies did not resolve cluster: {','.join(policy_list)}",
                    )
                )
    result.tracks.extend(spilled_tracks)
    if "fill" in policy_list:
        for track in result.tracks:
            if track.kind != "audio":
                fillers += _fill_gaps(track, fill_counter)
        if fillers:
            applied["fill"] += 1
    after = _span(result)
    result.declared_duration = after
    return ResolutionResult(
        timeline=result,
        before_samples=before,
        after_samples=after,
        unresolved=unresolved,
        applied=applied,
        fillers_added=fillers,
    )


def _clusters(segments: list[Segment]) -> list[list[Segment]]:
    ordered = sorted(segments, key=lambda item: (item.start, item.id))
    clusters: list[list[Segment]] = []
    for segment in ordered:
        if not clusters or segment.start >= max(item.end for item in clusters[-1]):
            clusters.append([segment])
        else:
            clusters[-1].append(segment)
    return clusters
