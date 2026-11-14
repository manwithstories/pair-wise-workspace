"""Frame-level expansion and deterministic schedule digests."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from fractions import Fraction
from typing import Any

from .model import Segment, Timeline, Track
from .timebase import SAMPLE_RATE, format_seconds, round_half_even


@dataclass
class ScheduleResult:
    report: dict[str, Any]

    @property
    def digest(self) -> str:
        return self.report["digest"]

    @property
    def schedule_frames(self) -> int:
        return self.report["schedule_frames"]


def _frame_start(index: int, fps: Fraction) -> int:
    return round_half_even(Fraction(index * SAMPLE_RATE, 1) / fps)


def _frame_count(duration: int, fps: Fraction) -> int:
    if duration <= 0:
        return 0
    return int(-(-(duration * fps) // SAMPLE_RATE))


def build_schedule(timeline: Timeline, fps: Any = None) -> ScheduleResult:
    rate = Fraction(fps) if fps is not None else timeline.fps
    if rate <= 0:
        raise ValueError("fps must be positive")
    duration = timeline.duration()
    frame_count = _frame_count(duration, rate)
    tracks = list(timeline.tracks)
    ordered_segments = {track.id: track.sorted_segments() for track in tracks}
    cursors = {track.id: 0 for track in tracks}
    active: dict[str, dict[str, Segment]] = {track.id: {} for track in tracks}
    frames: list[dict[str, Any]] = []
    overall_hash = hashlib.sha256()
    maximum = 0
    audio_maximum = 0
    for index in range(frame_count):
        start = _frame_start(index, rate)
        end = _frame_start(index + 1, rate)
        for track in tracks:
            ordered = ordered_segments[track.id]
            cursor = cursors[track.id]
            while cursor < len(ordered) and ordered[cursor].start < end:
                segment = ordered[cursor]
                if segment.duration > 0 and segment.end > start:
                    active[track.id][segment.id] = segment
                cursor += 1
            cursors[track.id] = cursor
            active[track.id] = {
                segment_id: segment
                for segment_id, segment in active[track.id].items()
                if segment.end > start
            }
        frame_active = _active_entries(tracks, active, start, end)
        count = len(frame_active)
        audio_count = sum(1 for entry in frame_active if entry["kind"] == "audio")
        maximum = max(maximum, count)
        audio_maximum = max(audio_maximum, audio_count)
        canonical = {
            "frame": index,
            "start_sample": start,
            "end_sample": end,
            "active": [
                {
                    "id": entry["id"],
                    "track": entry["track"],
                    "occluded_by": entry["occluded_by"],
                }
                for entry in frame_active
            ],
        }
        frame_digest = hashlib.sha256(
            json.dumps(canonical, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ).hexdigest()
        overall_hash.update(frame_digest.encode("ascii"))
        frames.append(
            {
                **canonical,
                "time_seconds": format_seconds(start),
                "active_count": count,
                "audio_count": audio_count,
                "digest": frame_digest,
            }
        )
    schedule_duration = _frame_start(frame_count, rate) if frame_count else 0
    drift = abs(schedule_duration - duration)
    report = {
        "fps": str(rate),
        "timeline_duration_samples": duration,
        "schedule_duration_samples": schedule_duration,
        "drift_samples": drift,
        "schedule_frames": frame_count,
        "max_concurrent": maximum,
        "max_audio_concurrent": audio_maximum,
        "digest": overall_hash.hexdigest(),
        "frames": frames,
    }
    return ScheduleResult(report=report)


def _active_entries(
    tracks: list[Track],
    active: dict[str, dict[str, Segment]],
    frame_start: int,
    frame_end: int,
) -> list[dict[str, Any]]:
    visual = sorted(
        (track for track in tracks if track.kind == "video"), key=lambda item: (item.z_order, item.id)
    )
    entries: list[dict[str, Any]] = []
    for track in tracks:
        for segment in active[track.id].values():
            occluded_by: list[str] = []
            if track.kind == "video":
                for upper in visual:
                    if (upper.z_order, upper.id) <= (track.z_order, track.id):
                        continue
                    for candidate in active[upper.id].values():
                        if candidate.start < segment.end and candidate.end > segment.start:
                            occluded_by.append(candidate.id)
            entries.append(
                {
                    "id": segment.id,
                    "track": track.id,
                    "kind": track.kind,
                    "start_sample": segment.start,
                    "end_sample": segment.end,
                    "occluded_by": sorted(occluded_by),
                }
            )
    return sorted(entries, key=lambda item: (item["track"], item["id"]))
