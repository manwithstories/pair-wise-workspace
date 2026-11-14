from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any

from .errors import CutplanError
from .interval import merge_intervals, overlap_pairs, scan_clusters
from .model import Clip, Timeline, Track
from .timebase import FrameInterval, round_half_up


_CLOSEUP_KINDS = {"closeup", "close-up", "close_up", "detail", "feature", "特写"}
_BEAT_KINDS = {"beat", "beat-hint", "beat_hint", "hint", "cue", "节拍", "节拍提示"}


def is_closeup(track: Track) -> bool:
    return track.kind.strip().lower() in _CLOSEUP_KINDS


def is_beat(track: Track) -> bool:
    return track.kind.strip().lower() in _BEAT_KINDS


def is_main(track: Track) -> bool:
    return track.kind.strip().lower() in {"main", "primary", "主画面", "主轨"}


def beat_bpm(timeline: Timeline) -> int:
    for source in (timeline.ext, timeline.manifest.ext):
        value = source.get("beat_bpm", source.get("bpm", 120))
        if isinstance(value, int) and value > 0:
            return value
    return 120


def nearest_beat(start: int, timeline: Timeline) -> tuple[int, int]:
    frames_per_beat = timeline.rate.fps * Fraction(60, beat_bpm(timeline))
    index = round_half_up(Fraction(start, 1) / frames_per_beat)
    frame = round_half_up(Fraction(index, 1) * frames_per_beat)
    return frame, abs(frame - start)


def closeup_violations(timeline: Timeline) -> list[dict[str, Any]]:
    main_intervals = merge_intervals(
        [clip.interval for track in timeline.tracks if is_main(track) for clip in track.clips]
    )
    violations: list[dict[str, Any]] = []
    for track in timeline.tracks:
        if not is_closeup(track):
            continue
        for clip in track.clips:
            if not any(
                main.start <= clip.start and clip.end <= main.end
                for main in main_intervals
            ):
                violations.append(
                    {
                        "track_id": track.id,
                        "clip_id": clip.id,
                        "interval": _interval_dict(clip.interval),
                    }
                )
    return violations


def beat_violations(timeline: Timeline) -> list[dict[str, Any]]:
    violations: list[dict[str, Any]] = []
    for track in timeline.tracks:
        if not is_beat(track):
            continue
        for clip in track.clips:
            nearest, distance = nearest_beat(clip.start, timeline)
            if distance > 2:
                violations.append(
                    {
                        "track_id": track.id,
                        "clip_id": clip.id,
                        "start": clip.start,
                        "nearest_beat": nearest,
                        "delta_frames": distance,
                    }
                )
    return violations


def _interval_dict(interval: FrameInterval) -> dict[str, int]:
    return {"start": interval.start, "end": interval.end}


@dataclass(slots=True)
class ValidationError:
    code: str
    path: str
    message: str = ""

    def as_dict(self) -> dict[str, str]:
        result = {"code": self.code, "path": self.path}
        if self.message:
            result["message"] = self.message
        return result


@dataclass(slots=True)
class ValidationReport:
    tracks: int = 0
    clips: int = 0
    conflicts: dict[str, Any] = field(default_factory=dict)
    zero_duration: list[dict[str, Any]] = field(default_factory=list)
    closeup: list[dict[str, Any]] = field(default_factory=list)
    beats: list[dict[str, Any]] = field(default_factory=list)
    audio_start_rounded: int = 0
    errors: list[ValidationError] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.errors

    def as_dict(self) -> dict[str, Any]:
        return {
            "ok": self.ok,
            "tracks": self.tracks,
            "clips": self.clips,
            "conflicts": self.conflicts,
            "zero_duration": {
                "count": len(self.zero_duration),
                "clips": self.zero_duration,
            },
            "constraints": {
                "closeup_outside_main": len(self.closeup),
                "beat_off_grid": len(self.beats),
                "closeup": self.closeup,
                "beats": self.beats,
            },
            "audio_start_rounded": self.audio_start_rounded,
            "errors": [error.as_dict() for error in self.errors],
        }


def validate_timeline(timeline: Timeline, *, include_overlap_errors: bool = True) -> ValidationReport:
    report = ValidationReport(tracks=len(timeline.tracks), clips=timeline.clip_count)
    report.audio_start_rounded = int(timeline.metadata.get("_audio_start_rounded", 0) or 0)
    _check_structural(timeline, report)
    report.conflicts = _conflicts(timeline)
    report.zero_duration = [
        {"track_id": track.id, "clip_id": clip.id, "start": clip.start}
        for track in timeline.tracks
        for clip in track.clips
        if clip.duration == 0
    ]
    report.closeup = closeup_violations(timeline)
    report.beats = beat_violations(timeline)
    if include_overlap_errors:
        for cluster in report.conflicts["cluster_details"]:
            report.errors.append(
                ValidationError(
                    "E_OVERLAP",
                    f"track:{cluster['track_id']}",
                    f"overlap cluster with {len(cluster['clips'])} clips",
                )
            )
    for clip in report.zero_duration:
        report.errors.append(
            ValidationError("E_ZERO_DURATION", f"clip:{clip['clip_id']}", "clip duration is zero")
        )
    for item in report.closeup:
        report.errors.append(
            ValidationError(
                "E_CLOSEUP_OUTSIDE_MAIN",
                f"clip:{item['clip_id']}",
                "closeup is not fully contained by a main-track clip",
            )
        )
    for item in report.beats:
        report.errors.append(
            ValidationError(
                "E_BEAT_OFF_GRID",
                f"clip:{item['clip_id']}",
                f"nearest beat is {item['delta_frames']} frames away (tolerance 2)",
            )
        )
    return report


validate = validate_timeline


def validate_file(path: str) -> ValidationReport:
    from .convert import load_timeline

    return validate_timeline(load_timeline(path))


def _check_structural(timeline: Timeline, report: ValidationReport) -> None:
    if timeline.declared_duration < 0:
        report.errors.append(ValidationError("E_DURATION_INVALID", "manifest", "negative duration"))
    seen_tracks: set[str] = set()
    seen_clips: set[str] = set()
    for track in timeline.tracks:
        if track.id in seen_tracks:
            report.errors.append(ValidationError("E_DUPLICATE_TRACK_ID", f"track:{track.id}", ""))
        seen_tracks.add(track.id)
        for clip in track.clips:
            if clip.start < 0 or clip.duration < 0:
                report.errors.append(ValidationError("E_CLIP_TIME_INVALID", f"clip:{clip.id}", ""))
            if clip.id in seen_clips:
                report.errors.append(ValidationError("E_DUPLICATE_CLIP_ID", f"clip:{clip.id}", ""))
            seen_clips.add(clip.id)


def _conflicts(timeline: Timeline) -> dict[str, Any]:
    cluster_details: list[dict[str, Any]] = []
    pair_details: list[dict[str, Any]] = []
    total_clusters = 0
    total_pairs = 0
    for track in timeline.tracks:
        ordered = track.sorted_clips
        intervals = [clip.interval for clip in ordered]
        clusters = scan_clusters(intervals)
        pairs = overlap_pairs(intervals)
        total_clusters += len(clusters)
        total_pairs += len(pairs)
        for indices in clusters:
            start = min(intervals[index].start for index in indices)
            end = max(intervals[index].end for index in indices)
            cluster_details.append(
                {
                    "track_id": track.id,
                    "clips": [ordered[index].id for index in indices],
                    "start": start,
                    "end": end,
                }
            )
        for first, second in pairs:
            pair_details.append(
                {
                    "track_id": track.id,
                    "first": ordered[first].id,
                    "second": ordered[second].id,
                }
            )
    return {
        "clusters": total_clusters,
        "pairs": total_pairs,
        "cluster_details": cluster_details,
        "pair_details": pair_details,
    }
