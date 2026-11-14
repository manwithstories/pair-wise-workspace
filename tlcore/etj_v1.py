"""ETJ v1: flat clips with whole-millisecond timing."""

from __future__ import annotations

from fractions import Fraction
from pathlib import Path
from typing import Any

from .io_utils import ExchangeError, extension_fields, integer, load_document, mapping, save_document, sequence, text
from .model import Segment, Timeline, Track
from .timebase import (
    is_ms_aligned,
    ms_to_samples,
    parse_rate,
    samples_to_bankers_ms,
    samples_to_ms,
)

TOP_KNOWN = {"format", "version", "name", "fps", "duration_ms", "tracks", "clips", "metadata", "extensions"}
TRACK_KNOWN = {"id", "kind", "z_order", "priority", "locked", "metadata", "extensions"}
CLIP_KNOWN = {"id", "track", "kind", "start_ms", "duration_ms", "source_in_ms", "priority", "locked", "metadata", "extensions"}


def read_v1(path: str | Path) -> Timeline:
    return timeline_from_v1(load_document(path))


def timeline_from_v1(data: dict[str, Any]) -> Timeline:
    if data.get("format") != "etj" or data.get("version") != 1:
        raise ExchangeError("not an ETJ v1 document")
    fps = parse_rate(data.get("fps", Fraction(24)))
    tracks_data = sequence(data.get("tracks", []), "tracks")
    clips_data = sequence(data.get("clips"), "clips")
    tracks: dict[str, Track] = {}
    for index, raw in enumerate(tracks_data):
        item = mapping(raw, f"tracks[{index}]")
        track = Track(
            id=text(item.get("id"), f"tracks[{index}].id"),
            kind=text(item.get("kind", "video"), f"tracks[{index}].kind"),
            z_order=integer(item.get("z_order", index), f"tracks[{index}].z_order"),
            priority=integer(item.get("priority", 0), f"tracks[{index}].priority"),
            locked=bool(item.get("locked", False)),
            metadata=dict(mapping(item.get("metadata", {}), f"tracks[{index}].metadata")),
            extensions=extension_fields(item, TRACK_KNOWN),
        )
        if track.id in tracks:
            raise ExchangeError(f"duplicate track id: {track.id}")
        tracks[track.id] = track
    for index, raw in enumerate(clips_data):
        item = mapping(raw, f"clips[{index}]")
        track_id = text(item.get("track"), f"clips[{index}].track")
        if track_id not in tracks:
            tracks[track_id] = Track(
                id=track_id,
                kind=text(item.get("kind", "video"), f"clips[{index}].kind"),
                z_order=integer(item.get("z_order", len(tracks)), f"clips[{index}].z_order"),
            )
        track = tracks[track_id]
        for field in ("start_ms", "duration_ms"):
            if field not in item:
                raise ExchangeError(f"clips[{index}] missing {field}")
        start = _whole_ms(item["start_ms"], f"clips[{index}].start_ms")
        duration = _whole_ms(item["duration_ms"], f"clips[{index}].duration_ms")
        source_in = _whole_ms(item.get("source_in_ms", 0), f"clips[{index}].source_in_ms")
        segment = Segment(
            id=text(item.get("id"), f"clips[{index}].id"),
            start=start,
            duration=duration,
            source_start=source_in,
            priority=integer(item.get("priority", track.priority), f"clips[{index}].priority"),
            locked=bool(item.get("locked", track.locked)),
            metadata=dict(mapping(item.get("metadata", {}), f"clips[{index}].metadata")),
            extensions=extension_fields(item, CLIP_KNOWN),
        )
        track.segments.append(segment)
    duration_ms = data.get("duration_ms")
    declared = None if duration_ms is None else _whole_ms(duration_ms, "duration_ms")
    timeline = Timeline(
        name=text(data.get("name", "timeline"), "name"),
        fps=fps,
        tracks=list(tracks.values()),
        metadata=dict(mapping(data.get("metadata", {}), "metadata")),
        extensions=extension_fields(data, TOP_KNOWN),
        declared_duration=declared,
    )
    return timeline


def _whole_ms(value: Any, label: str) -> int:
    try:
        number = Fraction(str(value))
    except (ValueError, ZeroDivisionError) as exc:
        raise ExchangeError(f"{label} is not a rational millisecond value") from exc
    if number.denominator != 1:
        raise ExchangeError(f"{label} must be a whole millisecond value")
    return ms_to_samples(int(number))


def write_v1(timeline: Timeline, path: str | Path) -> list[str]:
    document, warnings = timeline_to_v1(timeline)
    save_document(path, document)
    return warnings


def _quantized(samples: int, label: str, warnings: list[str]) -> int:
    if not is_ms_aligned(samples):
        warnings.append(
            f"{label}: {samples} samples ({samples_to_ms(samples)} ms) rounded to banker's millisecond"
        )
    return samples_to_bankers_ms(samples)


def timeline_to_v1(timeline: Timeline) -> tuple[dict[str, Any], list[str]]:
    warnings: list[str] = []
    tracks_data = []
    for track in timeline.tracks:
        item: dict[str, Any] = {
            "id": track.id,
            "kind": track.kind,
            "z_order": track.z_order,
            "priority": track.priority,
            "locked": track.locked,
            "metadata": dict(track.metadata),
        }
        for key, value in track.extensions.items():
            item.setdefault(key, value)
        tracks_data.append(item)
    clips_data = []
    for track, segment in timeline.iter_segments():
        item = {
            "id": segment.id,
            "track": track.id,
            "kind": track.kind,
            "start_ms": _quantized(segment.start, f"segment {segment.id}.start", warnings),
            "duration_ms": _quantized(segment.duration, f"segment {segment.id}.duration", warnings),
            "source_in_ms": _quantized(segment.source_start, f"segment {segment.id}.source_in", warnings),
            "priority": segment.priority,
            "locked": segment.locked,
            "metadata": dict(segment.metadata),
        }
        for key, value in segment.extensions.items():
            item.setdefault(key, value)
        clips_data.append(item)
    duration_warning_label = "timeline duration"
    duration_ms = (
        None
        if timeline.declared_duration is None
        else _quantized(timeline.declared_duration, duration_warning_label, warnings)
    )
    document: dict[str, Any] = {
        "format": "etj",
        "version": 1,
        "name": timeline.name,
        "fps": str(timeline.fps),
        "tracks": tracks_data,
        "clips": clips_data,
        "metadata": dict(timeline.metadata),
    }
    if duration_ms is not None:
        document["duration_ms"] = duration_ms
    for key, value in timeline.extensions.items():
        document.setdefault(key, value)
    return document, warnings
