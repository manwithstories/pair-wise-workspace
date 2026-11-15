"""OTIO 0.17 export/readback bridge with explicit stacking degradation."""

from __future__ import annotations

from fractions import Fraction
from pathlib import Path
from typing import Any

import opentimelineio as otio

from model import Clip, Timeline, rational_json, timeline_sort_key


class OtioBridgeError(ValueError):
    def __init__(self, code: str, message: str, *, exit_code: int = 3, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.exit_code = exit_code
        self.details = details


def _rt(frames: Fraction, rate: float) -> otio.opentime.RationalTime:
    return otio.opentime.RationalTime(float(frames), rate)


def _offline_metadata(clip: Clip) -> dict[str, Any]:
    payload = {
        "id": clip.id, "track": clip.track, "src": clip.src, "layer": clip.layer,
        "priority": clip.priority, "locked": clip.locked, "kind": clip.kind,
        "action": clip.action, "start": rational_json(clip.start),
        "duration": rational_json(clip.duration),
    }
    if clip.source_start is not None:
        payload["source_start"] = rational_json(clip.source_start)
    if clip.source_rate is not None:
        payload["source_rate"] = rational_json(clip.source_rate)
    return {"offline_resolve": payload}


def _degradation_counts(timeline: Timeline) -> tuple[int, dict[str, int]]:
    by_layer: dict[int, int] = {}
    for track in timeline.tracks:
        by_layer[track.layer] = by_layer.get(track.layer, 0) + 1
    degraded_layers = {str(layer): count - 1 for layer, count in sorted(by_layer.items()) if count > 1}
    return sum(degraded_layers.values()), degraded_layers


def build_otio(timeline: Timeline) -> tuple[otio.schema.Timeline, dict[str, Any]]:
    rate = float(timeline.rate)
    stack = otio.schema.Stack(name=timeline.name)
    omitted_zero_length = 0
    overlap_adjustments = 0
    for model_track in sorted(timeline.tracks, key=timeline_sort_key):
        track = otio.schema.Track(name=model_track.name, kind=otio.schema.Track.Kind.Video)
        track.metadata["offline_resolve"] = {
            "layer": model_track.layer, "priority": model_track.priority,
            "rate": rational_json(timeline.rate),
        }
        cursor = Fraction(0)
        for clip in sorted(model_track.clips, key=lambda item: (item.start, item.id)):
            if clip.duration == 0:
                omitted_zero_length += 1
                continue
            if clip.start < cursor:
                overlap_adjustments += 1
            elif clip.start > cursor:
                track.append(otio.schema.Gap(source_range=otio.opentime.TimeRange(
                    otio.opentime.RationalTime(0, rate), _rt(clip.start - cursor, rate))))
            duration = _rt(clip.duration, rate)
            if clip.kind == "gap":
                item: Any = otio.schema.Gap(source_range=otio.opentime.TimeRange(
                    otio.opentime.RationalTime(0, rate), duration), metadata=_offline_metadata(clip))
            else:
                item = otio.schema.Clip(
                    name=clip.src, source_range=otio.opentime.TimeRange(
                        otio.opentime.RationalTime(0, rate), duration),
                    metadata=_offline_metadata(clip),
                )
            track.append(item)
            cursor = max(cursor, clip.end)
        stack.append(track)
    otio_timeline = otio.schema.Timeline(name=timeline.name)
    otio_timeline.tracks = stack
    otio_timeline.global_start_time = otio.opentime.RationalTime(0, rate)
    degraded_count, degraded_layers = _degradation_counts(timeline)
    return otio_timeline, {
        "timeline_rate_float": rate, "degraded_track_count": degraded_count,
        "degraded_layers": degraded_layers, "zero_length_omitted": omitted_zero_length,
        "overlap_adjustments": overlap_adjustments,
    }


def _expected_tracks(timeline: Timeline) -> dict[str, list[Clip]]:
    return {track.name: list(sorted(track.clips, key=lambda item: (item.start, item.id)))
            for track in timeline.tracks}


def verify_otio(timeline: Timeline, readback: otio.schema.Timeline) -> list[dict[str, Any]]:
    mismatches: list[dict[str, Any]] = []
    expected = _expected_tracks(timeline)
    read_tracks = list(readback.tracks)
    if len(read_tracks) != len(expected):
        mismatches.append({"code": "track_count", "expected": len(expected), "actual": len(read_tracks)})
    by_name = {track.name: track for track in read_tracks}
    for name, clips in expected.items():
        otio_track = by_name.get(name)
        if otio_track is None:
            mismatches.append({"code": "missing_track", "track": name})
            continue
        expected_clips = [clip for clip in clips if clip.kind == "clip" and clip.duration > 0]
        actual_items = [item for item in otio_track if isinstance(item, otio.schema.Clip)]
        if len(actual_items) != len(expected_clips):
            mismatches.append({"code": "clip_count", "track": name,
                               "expected": len(expected_clips), "actual": len(actual_items)})
            continue
        for expected_clip, item in zip(expected_clips, actual_items):
            offline = item.metadata.get("offline_resolve", {})
            for field in ("id", "src", "layer", "priority", "locked", "action", "start", "duration"):
                if offline.get(field) != (rational_json(expected_clip.start) if field == "start" else
                                          rational_json(expected_clip.duration) if field == "duration" else
                                          getattr(expected_clip, field)):
                    mismatches.append({"code": "clip_boundary", "track": name, "clip": expected_clip.id,
                                       "field": field, "expected": getattr(expected_clip, field, None),
                                       "actual": offline.get(field)})
        expected_gaps = [clip for clip in clips if clip.kind == "gap" and clip.duration > 0]
        actual_gaps = [item for item in otio_track if isinstance(item, otio.schema.Gap) and
                       item.metadata.get("offline_resolve", {}).get("kind") == "gap"]
        if len(actual_gaps) != len(expected_gaps):
            mismatches.append({"code": "gap_count", "track": name,
                               "expected": len(expected_gaps), "actual": len(actual_gaps)})
        for expected_gap, item in zip(expected_gaps, actual_gaps):
            offline = item.metadata.get("offline_resolve", {})
            if (offline.get("id") != expected_gap.id or
                    offline.get("start") != rational_json(expected_gap.start) or
                    offline.get("duration") != rational_json(expected_gap.duration)):
                mismatches.append({"code": "gap_boundary", "track": name, "gap": expected_gap.id})
    expected_duration = max((clip.end for clip in timeline.all_clips()), default=Fraction(0))
    actual_duration = readback.tracks.duration().to_frames() if readback.tracks else 0
    if abs(float(expected_duration) - float(actual_duration)) > 1e-6:
        mismatches.append({"code": "duration", "expected_frames": rational_json(expected_duration),
                           "actual_frames": actual_duration})
    return mismatches


def export_otio(timeline: Timeline, path: str | Path, *, verify: bool = True) -> dict[str, Any]:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    otio_timeline, build_info = build_otio(timeline)
    try:
        otio.adapters.write_to_file(otio_timeline, str(path))
        readback = otio.adapters.read_from_file(str(path)) if verify else None
    except Exception as exc:
        raise OtioBridgeError("otio_io_error", f"OTIO write/read failed: {exc}") from exc
    mismatches = verify_otio(timeline, readback) if readback is not None else []
    clip_count = sum(1 for track in timeline.tracks for clip in track.clips
                     if clip.kind == "clip" and clip.duration > 0)
    gap_count = sum(1 for track in timeline.tracks for clip in track.clips if clip.kind == "gap")
    return {
        "status": "verified" if verify and not mismatches else ("exported" if not verify else "mismatch"),
        "path": path.name,
        "track_count": len(timeline.tracks),
        "clip_count": clip_count,
        "gap_count": gap_count,
        "duration_frames": rational_json(max((clip.end for clip in timeline.all_clips()),
                                             default=Fraction(0))),
        "verified": bool(verify and not mismatches),
        "mismatches": mismatches,
        "exit_code": 0 if not verify or not mismatches else 3,
        **build_info,
    }
