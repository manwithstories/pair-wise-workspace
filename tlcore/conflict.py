"""Deterministic conflict detection and cross-track statistics."""

from __future__ import annotations

import heapq
from dataclasses import dataclass, field
from typing import Any

from .intervals import Interval, merge_intervals, overlap
from .model import Segment, Timeline, Track
from .timebase import format_seconds


@dataclass(frozen=True)
class Conflict:
    kind: str
    track_id: str
    segment_ids: tuple[str, ...]
    start: int
    end: int
    overlap_samples: int = 0
    unresolvable: bool = False
    reason: str | None = None

    def as_dict(self) -> dict[str, Any]:
        data: dict[str, Any] = {
            "kind": self.kind,
            "track": self.track_id,
            "segment_ids": list(self.segment_ids),
            "start_sample": self.start,
            "end_sample": self.end,
            "overlap_samples": self.overlap_samples,
            "overlap_seconds": format_seconds(self.overlap_samples),
        }
        if self.unresolvable:
            data["unresolvable"] = True
            data["reason"] = self.reason
        return data


@dataclass
class ConflictReport:
    conflicts: list[Conflict] = field(default_factory=list)
    occlusions: list[dict[str, Any]] = field(default_factory=list)
    audio_peak: int = 0
    audio_segment_count: int = 0
    track_count: int = 0
    segment_count: int = 0

    @property
    def pairs(self) -> list[Conflict]:
        return [item for item in self.conflicts if len(item.segment_ids) == 2]

    @property
    def conflict_count(self) -> int:
        return len(self.conflicts)

    def as_dict(self) -> dict[str, Any]:
        return {
            "track_count": self.track_count,
            "segment_count": self.segment_count,
            "conflict_count": self.conflict_count,
            "pair_count": len(self.pairs),
            "pairs": [item.as_dict() for item in self.pairs],
            "invalid": [
                item.as_dict()
                for item in self.conflicts
                if item.kind in ("zero_length", "negative_length", "out_of_bounds")
            ],
            "occlusion_count": len(self.occlusions),
            "occlusions": self.occlusions,
            "audio_segment_count": self.audio_segment_count,
            "audio_concurrency_peak": self.audio_peak,
        }


def _invalid_conflicts(timeline: Timeline) -> list[Conflict]:
    result: list[Conflict] = []
    declared = timeline.declared_duration
    for track, segment in timeline.iter_segments():
        if segment.duration == 0:
            result.append(
                Conflict("zero_length", track.id, (segment.id,), segment.start, segment.end)
            )
        elif segment.duration < 0:
            result.append(
                Conflict("negative_length", track.id, (segment.id,), segment.start, segment.end)
            )
        outside = segment.start < 0 or (declared is not None and segment.end > declared)
        if outside:
            start = segment.start
            end = max(segment.end, start)
            result.append(
                Conflict("out_of_bounds", track.id, (segment.id,), start, end)
            )
    return result


def _track_overlaps(timeline: Timeline, track: Track) -> list[Conflict]:
    if track.kind == "audio":
        return []
    segments = [
        segment
        for segment in sorted(track.segments, key=lambda item: (item.start, item.id))
        if segment.duration > 0
    ]
    active: dict[str, Segment] = {}
    endings: list[tuple[int, str]] = []
    result: list[Conflict] = []
    for segment in segments:
        while endings and endings[0][0] <= segment.start:
            _, finished_id = heapq.heappop(endings)
            active.pop(finished_id, None)
        for other in active.values():
            amount = overlap(other.interval, segment.interval)
            if amount <= 0:
                continue
            first, second = sorted((other, segment), key=lambda item: (item.start, item.id))
            locked = timeline.effective_locked(track, first) or timeline.effective_locked(
                track, second
            )
            reason = "locked segment cannot move" if locked else None
            result.append(
                Conflict(
                    "same_track_overlap",
                    track.id,
                    (first.id, second.id),
                    max(first.start, second.start),
                    min(first.end, second.end),
                    amount,
                    locked,
                    reason,
                )
            )
        active[segment.id] = segment
        heapq.heappush(endings, (segment.end, segment.id))
    return result


def _cross_track_stats(timeline: Timeline) -> tuple[list[dict[str, Any]], int, int]:
    occlusions: list[dict[str, Any]] = []
    visual = [track for track in timeline.sorted_tracks() if track.kind == "video"]
    for lower_index, lower in enumerate(visual):
        higher = visual[lower_index + 1 :]
        if not higher:
            continue
        regions: list[tuple[Interval, str]] = []
        for higher_track in higher:
            intervals = [
                segment.interval for segment in higher_track.segments if segment.duration > 0
            ]
            regions.extend((item, higher_track.id) for item in merge_intervals(intervals))
        for segment in lower.segments:
            if segment.duration <= 0:
                continue
            for region, higher_id in regions:
                start = max(segment.start, region.start)
                end = min(segment.end, region.end)
                if start < end:
                    occlusions.append(
                        {
                            "lower_track": lower.id,
                            "lower_segment": segment.id,
                            "upper_track": higher_id,
                            "start_sample": start,
                            "end_sample": end,
                            "duration_samples": end - start,
                        }
                    )
    audio_tracks = [track for track in timeline.tracks if track.kind == "audio"]
    events: list[tuple[int, int]] = []
    audio_count = 0
    for track in audio_tracks:
        for segment in track.segments:
            if segment.duration <= 0:
                continue
            audio_count += 1
            events.append((segment.start, 1))
            events.append((segment.end, -1))
    active = 0
    peak = 0
    for _, change in sorted(events, key=lambda item: (item[0], item[1])):
        active += change
        peak = max(peak, active)
    return occlusions, peak, audio_count


def scan_conflicts(timeline: Timeline, *, include_occlusions: bool = True) -> ConflictReport:
    conflicts = _invalid_conflicts(timeline)
    for track in timeline.tracks:
        conflicts.extend(_track_overlaps(timeline, track))
    conflicts.sort(
        key=lambda item: (
            item.start,
            item.track_id,
            item.segment_ids,
            item.kind,
        )
    )
    occlusions: list[dict[str, Any]] = []
    peak = 0
    audio_count = 0
    if include_occlusions:
        occlusions, peak, audio_count = _cross_track_stats(timeline)
    return ConflictReport(
        conflicts=conflicts,
        occlusions=occlusions,
        audio_peak=peak,
        audio_segment_count=audio_count,
        track_count=len(timeline.tracks),
        segment_count=timeline.segment_count(),
    )
