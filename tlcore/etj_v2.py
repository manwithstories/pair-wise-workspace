"""ETJ v2: layered tracks, inherited attributes and sample offsets."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from .io_utils import ExchangeError, extension_fields, integer, load_document, mapping, save_document, sequence, text
from .model import Segment, Timeline, Track
from .timebase import SAMPLE_RATE, parse_rate, parse_samples, time_to_json

TOP_KNOWN = {
    "format", "version", "name", "fps", "timebase", "metadata",
    "extensions", "duration", "defaults", "tracks",
}
TRACK_KNOWN = {
    "id", "kind", "z_order", "priority", "locked", "metadata",
    "extensions", "defaults", "origin", "segments",
}
SEGMENT_KNOWN = {
    "id", "start", "offset", "duration", "source_in", "priority",
    "locked", "metadata", "extensions",
}


def read_v2(path: str | Path) -> Timeline:
    return timeline_from_v2(load_document(path))


def _defaults(value: Any, label: str, parent: dict[str, Any] | None = None) -> dict[str, Any]:
    item = mapping(value, label)
    base = dict(parent or {})
    result = {
        "priority": integer(item.get("priority", base.get("priority", 0)), f"{label}.priority"),
        "locked": bool(item.get("locked", base.get("locked", False))),
        "metadata": dict(base.get("metadata", {})),
        "extensions": dict(base.get("extensions", {})),
    }
    result["metadata"].update(mapping(item.get("metadata", {}), f"{label}.metadata"))
    result["extensions"].update(mapping(item.get("extensions", {}), f"{label}.extensions"))
    return result


def timeline_from_v2(data: dict[str, Any]) -> Timeline:
    if data.get("format") != "etj" or data.get("version") != 2:
        raise ExchangeError("not an ETJ v2 document")
    if integer(data.get("timebase", SAMPLE_RATE), "timebase") != SAMPLE_RATE:
        raise ExchangeError(f"unsupported timebase: {data.get('timebase')}")
    fps = parse_rate(data.get("fps", 24))
    timeline_defaults = _defaults(data.get("defaults", {}), "defaults")
    tracks: list[Track] = []
    track_ids: set[str] = set()
    for track_index, raw in enumerate(sequence(data.get("tracks", []), "tracks")):
        item = mapping(raw, f"tracks[{track_index}]")
        track_defaults = _defaults(
            item.get("defaults", {}), f"tracks[{track_index}].defaults", timeline_defaults
        )
        track_id = text(item.get("id"), f"tracks[{track_index}].id")
        if track_id in track_ids:
            raise ExchangeError(f"duplicate track id: {track_id}")
        track_ids.add(track_id)
        priority = integer(
            item.get("priority", track_defaults["priority"]),
            f"tracks[{track_index}].priority",
        )
        locked = bool(item.get("locked", track_defaults["locked"]))
        metadata = dict(track_defaults["metadata"])
        metadata.update(mapping(item.get("metadata", {}), f"tracks[{track_index}].metadata"))
        extensions = dict(track_defaults["extensions"])
        extensions.update(extension_fields(item, TRACK_KNOWN))
        track = Track(
            id=track_id,
            kind=text(item.get("kind", "video"), f"tracks[{track_index}].kind"),
            z_order=integer(item.get("z_order", track_index), f"tracks[{track_index}].z_order"),
            priority=priority,
            locked=locked,
            metadata=metadata,
            extensions=extensions,
        )
        origin = parse_samples(item.get("origin", {"samples": 0}), fps)
        cursor = origin
        for segment_index, raw_segment in enumerate(
            sequence(item.get("segments", []), f"tracks[{track_index}].segments")
        ):
            segment_data = mapping(raw_segment, f"tracks[{track_index}].segments[{segment_index}]")
            if "start" in segment_data and "offset" in segment_data:
                raise ExchangeError(f"segment {segment_data.get('id')}: start and offset are exclusive")
            if "start" in segment_data:
                start = parse_samples(segment_data["start"], fps)
            elif "offset" in segment_data:
                start = cursor + parse_samples(segment_data["offset"], fps)
            else:
                start = cursor
            if "duration" not in segment_data:
                raise ExchangeError(f"segment {segment_data.get('id')}: missing duration")
            duration = parse_samples(segment_data["duration"], fps)
            cursor = start + duration
            segment_metadata = dict(track_defaults["metadata"])
            segment_metadata.update(mapping(segment_data.get("metadata", {}), "segment.metadata"))
            segment_extensions = dict(track_defaults["extensions"])
            segment_extensions.update(extension_fields(segment_data, SEGMENT_KNOWN))
            track.segments.append(
                Segment(
                    id=text(segment_data.get("id"), f"tracks[{track_index}].segments[{segment_index}].id"),
                    start=start,
                    duration=duration,
                    source_start=parse_samples(segment_data.get("source_in", {"samples": 0}), fps),
                    priority=integer(segment_data.get("priority", track.priority), "segment.priority"),
                    locked=bool(segment_data.get("locked", track.locked)),
                    metadata=segment_metadata,
                    extensions=segment_extensions,
                )
            )
        tracks.append(track)
    declared = data.get("duration")
    return Timeline(
        name=text(data.get("name", "timeline"), "name"),
        fps=fps,
        tracks=tracks,
        metadata=dict(mapping(data.get("metadata", {}), "metadata")),
        extensions=extension_fields(data, TOP_KNOWN),
        declared_duration=None if declared is None else parse_samples(declared, fps),
    )


def write_v2(timeline: Timeline, path: str | Path) -> None:
    save_document(path, timeline_to_v2(timeline))


def timeline_to_v2(timeline: Timeline) -> dict[str, Any]:
    tracks_data: list[dict[str, Any]] = []
    for track in timeline.tracks:
        track_data: dict[str, Any] = {
            "id": track.id,
            "kind": track.kind,
            "z_order": track.z_order,
            "origin": time_to_json(0),
            "defaults": {
                "priority": track.priority,
                "locked": track.locked,
                "metadata": dict(track.metadata),
            },
            "segments": [],
        }
        cursor = 0
        for segment in track.sorted_segments():
            segment_data: dict[str, Any] = {
                "id": segment.id,
                "offset": time_to_json(segment.start - cursor),
                "duration": time_to_json(segment.duration),
                "source_in": time_to_json(segment.source_start),
            }
            if segment.priority != track.priority:
                segment_data["priority"] = segment.priority
            if segment.locked != track.locked:
                segment_data["locked"] = segment.locked
            segment_data["metadata"] = dict(segment.metadata)
            for key, value in segment.extensions.items():
                segment_data.setdefault(key, value)
            track_data["segments"].append(segment_data)
            cursor = segment.end
        for key, value in track.extensions.items():
            track_data.setdefault(key, value)
        tracks_data.append(track_data)
    document: dict[str, Any] = {
        "format": "etj",
        "version": 2,
        "name": timeline.name,
        "fps": str(timeline.fps),
        "timebase": SAMPLE_RATE,
        "defaults": {"priority": 0, "locked": False},
        "tracks": tracks_data,
        "metadata": dict(timeline.metadata),
    }
    if timeline.declared_duration is not None:
        document["duration"] = time_to_json(timeline.declared_duration)
    for key, value in timeline.extensions.items():
        document.setdefault(key, value)
    return document
