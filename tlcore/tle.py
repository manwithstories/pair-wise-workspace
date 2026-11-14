"""Timeline Exchange (TLE) v1/v2 readers and writers.

The reader accepts both the flat nanosecond v1 form and the rational/frame
v2 form.  Parsing is deliberately strict about exact values; a malformed
document raises :class:`TLEError` containing source file and clip identity.
"""

from __future__ import annotations

import json
from fractions import Fraction
from pathlib import Path
from typing import Any

from .model import Clip, LinkGroup, Marker, ModelError, Timeline, Track, Transition
from .timebase import TimebaseError, ns_to_seconds, parse_rate, parse_time


class TLEError(ValueError):
    def __init__(self, message: str, *, source: str = "<memory>", clip_id: str | None = None):
        self.source = source
        self.clip_id = clip_id
        prefix = source
        if clip_id is not None:
            prefix += f": clip {clip_id!r}"
        super().__init__(f"{prefix}: {message}")


def _fail(message: str, source: str, clip_id: str | None = None) -> TLEError:
    return TLEError(message, source=source, clip_id=clip_id)


def _require(value: dict[str, Any], names: tuple[str, ...], what: str, source: str,
             clip_id: str | None = None) -> Any:
    for name in names:
        if name in value:
            return value[name]
    raise _fail(f"missing required {what} (expected one of {', '.join(names)})",
                source, clip_id)


def _time(value: Any, version: int, fps: Fraction | None, source: str,
          clip_id: str | None, field: str) -> int:
    try:
        return parse_time(value, version=version, fps=fps, field=field)
    except TimebaseError as exc:
        raise _fail(str(exc), source, clip_id) from exc


def _rate(value: Any, source: str) -> Fraction:
    try:
        return parse_rate(value)
    except TimebaseError as exc:
        raise _fail(str(exc), source) from exc


def _track_kind(track_id: str, declared: Any = None) -> str:
    if declared:
        return str(declared)
    lowered = track_id.lower()
    for key in ("video", "audio", "subtitle", "music", "口播", "字幕", "音乐"):
        if key in lowered:
            if key in ("music", "音乐"):
                return "audio"
            if key in ("口播",):
                return "audio"
            if key in ("字幕",):
                return "subtitle"
            return key
    return "video"


def _clip_source(clip_data: dict[str, Any], version: int, fps: Fraction | None,
                 source: str, clip_id: str) -> tuple[int, int | None, int | None]:
    source_value = clip_data.get("source", clip_data.get("source_interval"))
    source_in = clip_data.get("source_in_ns", clip_data.get("source_in", 0))
    source_out: Any = None
    available: Any = clip_data.get("source_duration_ns")
    for key in (
        "source_duration", "media_duration_ns", "media_duration",
        "asset_duration_ns", "material_duration_ns", "source_length_ns",
    ):
        if available is None and key in clip_data:
            available = clip_data[key]
    if source_value is not None:
        if not isinstance(source_value, dict):
            raise _fail("source must be an object with in/out", source, clip_id)
        if "in" in source_value or "in_ns" in source_value:
            source_in = source_value.get("in", source_value.get("in_ns"))
        if "out" in source_value or "out_ns" in source_value:
            source_out = source_value.get("out", source_value.get("out_ns"))
        for key in ("available", "available_ns", "duration_ns", "media_duration"):
            if key in source_value:
                available = source_value[key]
                break
    if "source_out" in clip_data or "source_out_ns" in clip_data:
        source_out = clip_data.get("source_out", clip_data.get("source_out_ns"))
    source_in_ns = _time(source_in, version, fps, source, clip_id, "source.in")
    source_out_ns = None
    if source_out is not None:
        source_out_ns = _time(source_out, version, fps, source, clip_id, "source.out")
        if source_out_ns < source_in_ns:
            raise _fail("source out point precedes in point", source, clip_id)
    available_ns = None
    if available is not None:
        available_ns = _time(available, version, fps, source, clip_id, "source.available")
        if available_ns < 0:
            raise _fail("source available duration must not be negative", source, clip_id)
    return source_in_ns, source_out_ns, available_ns


def _parse_clip(clip_data: Any, track_id: str, kind: str, version: int,
                fps: Fraction | None, source: str) -> Clip:
    if not isinstance(clip_data, dict):
        raise _fail(f"track {track_id}: clip must be an object", source)
    clip_id_value = clip_data.get("id", clip_data.get("clip_id"))
    if not clip_id_value:
        raise _fail(f"track {track_id}: clip is missing id", source, str(clip_id_value) if clip_id_value else None)
    clip_id = str(clip_id_value)
    start_value = _require(clip_data, ("start", "start_ns", "in", "in_ns"),
                           "start time", source, clip_id)
    start_ns = _time(start_value, version, fps, source, clip_id, "start")
    if start_ns < 0:
        raise _fail("start time must not be negative", source, clip_id)
    source_in_ns, source_out_ns, available_ns = _clip_source(
        clip_data, version, fps, source, clip_id
    )
    duration_value = clip_data.get("duration", clip_data.get("duration_ns"))
    end_value = clip_data.get(
        "end", clip_data.get("end_ns", clip_data.get("out", clip_data.get("out_ns")))
    )
    if duration_value is not None:
        duration_ns = _time(duration_value, version, fps, source, clip_id, "duration")
    elif source_out_ns is not None:
        duration_ns = source_out_ns - source_in_ns
    elif end_value is not None:
        end_ns = _time(end_value, version, fps, source, clip_id, "end")
        duration_ns = end_ns - start_ns
    else:
        raise _fail("missing required duration, source.out, or end", source, clip_id)
    if duration_ns < 0:
        raise _fail("clip duration must not be negative", source, clip_id)
    link_value = clip_data.get(
        "link", clip_data.get("link_id", clip_data.get("link_group"))
    )
    if isinstance(link_value, dict):
        link_value = link_value.get("id")
    link_id = str(link_value) if link_value else None
    priority = clip_data.get("priority", 0)
    if isinstance(priority, bool) or not isinstance(priority, int):
        raise _fail("priority must be an integer", source, clip_id)
    metadata_value = clip_data.get("metadata", {})
    if not isinstance(metadata_value, dict):
        raise _fail("clip metadata must be an object", source, clip_id)
    return Clip(
        id=clip_id,
        track=track_id,
        kind=str(clip_data.get("kind", kind)),
        start_ns=start_ns,
        duration_ns=duration_ns,
        source_in_ns=source_in_ns,
        source_duration_ns=available_ns,
        link_id=link_id,
        priority=priority,
        metadata=dict(metadata_value),
    )


def _parse_links(raw_links: Any, source: str) -> list[LinkGroup]:
    if raw_links is None:
        return []
    groups: list[LinkGroup] = []
    if isinstance(raw_links, dict):
        items = [{"id": key, "clips": value} for key, value in raw_links.items()]
    elif isinstance(raw_links, list):
        items = raw_links
    else:
        raise _fail("links must be an object or array", source)
    for item in items:
        if not isinstance(item, dict):
            raise _fail("link group must be an object", source)
        group_id = item.get("id", item.get("link_id"))
        clip_ids = item.get("clips", item.get("clip_ids", item.get("members")))
        if not group_id or not isinstance(clip_ids, list):
            raise _fail("link group requires id and clips array", source,
                        str(group_id) if group_id else None)
        normalized = [
            str(clip_id.get("id", clip_id.get("clip_id")))
            if isinstance(clip_id, dict) else str(clip_id)
            for clip_id in clip_ids
        ]
        groups.append(LinkGroup(str(group_id), normalized))
    return groups


def _parse_transition(item: Any, version: int, fps: Fraction | None, source: str) -> Transition:
    if not isinstance(item, dict):
        raise _fail("transition must be an object", source)
    trans_id = item.get("id", item.get("transition_id"))
    if not trans_id:
        raise _fail("transition is missing id", source)
    from_id = item.get("from", item.get("from_clip", item.get("from_clip_id")))
    to_id = item.get("to", item.get("to_clip", item.get("to_clip_id")))
    between = item.get("between")
    if (not from_id or not to_id) and isinstance(between, list) and len(between) == 2:
        from_id, to_id = between
    if isinstance(from_id, dict):
        from_id = from_id.get("id", from_id.get("clip_id"))
    if isinstance(to_id, dict):
        to_id = to_id.get("id", to_id.get("clip_id"))
    if not from_id or not to_id:
        raise _fail("transition requires from and to clip ids", source, str(trans_id))
    track_id = item.get("track")
    if not track_id:
        raise _fail("transition requires track", source, str(trans_id))
    duration = _time(_require(item, ("duration", "duration_ns"), "duration",
                              source, str(trans_id)), version, fps, source, str(trans_id),
                      "transition.duration")
    max_duration = None
    raw_max = item.get("max", item.get("max_duration", item.get("limit")))
    if raw_max is not None:
        max_duration = _time(raw_max, version, fps, source, str(trans_id), "transition.max")
    return Transition(
        id=str(trans_id),
        track=str(track_id),
        from_clip_id=str(from_id),
        to_clip_id=str(to_id),
        duration_ns=duration,
        max_duration_ns=max_duration,
        metadata=dict(item.get("metadata", {})) if isinstance(item.get("metadata", {}), dict) else {},
    )


def _parse_marker(item: Any, version: int, fps: Fraction | None, source: str) -> Marker:
    if not isinstance(item, dict):
        raise _fail("marker must be an object", source)
    marker_id = item.get("id")
    if not marker_id:
        raise _fail("marker is missing id", source)
    start = _time(_require(item, ("start", "at", "time", "start_ns"), "start", source, str(marker_id)),
                  version, fps, source, str(marker_id), "marker.start")
    duration = 0
    if "duration" in item or "duration_ns" in item:
        duration = _time(item.get("duration", item.get("duration_ns")), version, fps,
                         source, str(marker_id), "marker.duration")
    metadata: Any = item.get("metadata", {})
    if not isinstance(metadata, dict):
        raise _fail("marker metadata must be an object", source, str(marker_id))
    return Marker(
        id=str(marker_id),
        track=str(item["track"]) if item.get("track") is not None else None,
        start_ns=start,
        duration_ns=duration,
        name=str(item.get("name", "")),
        metadata=dict(metadata),
    )


def parse_timeline_document(document: Any, *, source: str = "<memory>") -> Timeline:
    if not isinstance(document, dict):
        raise _fail("root document must be a JSON object", source)
    raw_version = document.get("version", document.get("tle_version"))
    if raw_version is None:
        raw_version = 1
    try:
        version = int(raw_version)
    except (TypeError, ValueError) as exc:
        raise _fail("version must be 1 or 2", source) from exc
    if version not in (1, 2):
        raise _fail(f"unsupported version: {raw_version!r}", source)
    fps = _rate(document.get("fps", 25), source) if "fps" in document else Fraction(25, 1)
    declared_value = document.get(
        "declared_duration", document.get(
            "declared_duration_ns", document.get("duration", document.get("duration_ns"))
        )
    )
    if declared_value is None:
        raise _fail("missing declared duration", source)
    declared_ns = _time(declared_value, version, fps, source, None, "duration")
    if declared_ns < 0:
        raise _fail("declared duration must not be negative", source)
    raw_tracks = document.get("tracks")
    if not isinstance(raw_tracks, list):
        raise _fail("tracks must be an array", source)
    tracks: list[Track] = []
    markers: list[Marker] = []
    for track_data in raw_tracks:
        if not isinstance(track_data, dict):
            raise _fail("track must be an object", source)
        track_id = track_data.get("id", track_data.get("name"))
        if not track_id:
            raise _fail("track is missing id", source)
        track_id = str(track_id)
        kind = _track_kind(track_id, track_data.get("kind"))
        raw_clips = track_data.get("clips")
        if not isinstance(raw_clips, list):
            raise _fail("track clips must be an array", source, track_id)
        clips = [_parse_clip(clip, track_id, kind, version, fps, source) for clip in raw_clips]
        for raw_marker in track_data.get("markers", []):
            marker = _parse_marker(raw_marker, version, fps, source)
            if marker.track is None:
                marker.track = track_id
            markers.append(marker)
        tracks.append(Track(track_id, kind, clips, dict(track_data.get("metadata", {}))
                             if isinstance(track_data.get("metadata", {}), dict) else {}))
    raw_transitions = document.get("transitions", [])
    if not isinstance(raw_transitions, list):
        raise _fail("transitions must be an array", source)
    transitions = [_parse_transition(item, version, fps, source) for item in raw_transitions]
    raw_markers = document.get("markers", [])
    if not isinstance(raw_markers, list):
        raise _fail("markers must be an array", source)
    markers.extend(_parse_marker(item, version, fps, source) for item in raw_markers)
    timeline = Timeline(
        version=version,
        fps=fps,
        declared_duration_ns=declared_ns,
        tracks=tracks,
        transitions=transitions,
        link_groups=_parse_links(document.get("links"), source),
        markers=markers,
        metadata=dict(document.get("metadata", {})) if isinstance(document.get("metadata", {}), dict) else {},
    )
    try:
        timeline.validate()
    except ModelError as exc:
        raise _fail(str(exc), source) from exc
    return timeline


def read_tle(path: str | Path) -> Timeline:
    source = str(path)
    try:
        text = Path(path).read_text(encoding="utf-8")
    except (OSError, UnicodeError) as exc:
        raise _fail(f"cannot read file: {exc}", source) from exc
    try:
        document = json.loads(text)
    except json.JSONDecodeError as exc:
        raise _fail(f"invalid JSON at line {exc.lineno}, column {exc.colno}: {exc.msg}",
                    source) from exc
    return parse_timeline_document(document, source=source)


def _time_value(ns: int, version: int) -> int | dict[str, int]:
    if version == 1:
        return ns
    value = ns_to_seconds(ns)
    return {"num": value.numerator, "den": value.denominator}


def to_v1_document(timeline: Timeline) -> tuple[dict[str, Any], list[str]]:
    warnings: list[str] = []
    if timeline.link_groups:
        warnings.append(
            f"v2 link groups downgraded to v1: {len(timeline.link_groups)} group(s)"
        )
    if timeline.transitions:
        warnings.append(
            f"v2 transitions downgraded to v1: {len(timeline.transitions)} transition(s)"
        )
    if timeline.markers:
        warnings.append(f"v2 markers downgraded to v1: {len(timeline.markers)} marker(s)")
    tracks: list[dict[str, Any]] = []
    for track in timeline.tracks:
        clips: list[dict[str, Any]] = []
        for clip in track.ordered_clips():
            item: dict[str, Any] = {
                "id": clip.id,
                "start_ns": clip.start_ns,
                "duration_ns": clip.duration_ns,
                "source_in_ns": clip.source_in_ns,
                "kind": clip.kind,
                "priority": clip.priority,
            }
            if clip.source_duration_ns is not None:
                item["source_duration_ns"] = clip.source_duration_ns
            if clip.metadata:
                item["metadata"] = dict(clip.metadata)
            clips.append(item)
        track_item: dict[str, Any] = {"id": track.id, "kind": track.kind, "clips": clips}
        if track.metadata:
            track_item["metadata"] = dict(track.metadata)
        tracks.append(track_item)
    document: dict[str, Any] = {
        "version": 1,
        "duration_ns": timeline.declared_duration_ns,
        "tracks": tracks,
    }
    if timeline.metadata:
        document["metadata"] = dict(timeline.metadata)
    return document, warnings


def to_v2_document(timeline: Timeline) -> dict[str, Any]:
    tracks: list[dict[str, Any]] = []
    for track in timeline.tracks:
        clips: list[dict[str, Any]] = []
        for clip in track.ordered_clips():
            item: dict[str, Any] = {
                "id": clip.id,
                "start": _time_value(clip.start_ns, 2),
                "duration": _time_value(clip.duration_ns, 2),
                "source": {
                    "in": _time_value(clip.source_in_ns, 2),
                    "out": _time_value(clip.source_out_ns, 2),
                },
                "kind": clip.kind,
                "priority": clip.priority,
            }
            if clip.source_duration_ns is not None:
                item["source_duration"] = _time_value(clip.source_duration_ns, 2)
            if clip.link_id:
                item["link"] = clip.link_id
            if clip.metadata:
                item["metadata"] = dict(clip.metadata)
            clips.append(item)
        track_item: dict[str, Any] = {"id": track.id, "kind": track.kind, "clips": clips}
        if track.metadata:
            track_item["metadata"] = dict(track.metadata)
        tracks.append(track_item)
    fps = timeline.fps
    document: dict[str, Any] = {
        "version": 2,
        "fps": {"num": fps.numerator, "den": fps.denominator},
        "duration": _time_value(timeline.declared_duration_ns, 2),
        "tracks": tracks,
        "links": [
            {"id": group.id, "clips": list(group.clip_ids)}
            for group in sorted(timeline.link_groups, key=lambda item: item.id)
        ],
        "transitions": [
            {
                "id": item.id,
                "track": item.track,
                "from": item.from_clip_id,
                "to": item.to_clip_id,
                "duration": _time_value(item.duration_ns, 2),
                **(
                    {"max": _time_value(item.max_duration_ns, 2)}
                    if item.max_duration_ns is not None
                    else {}
                ),
                **({"metadata": dict(item.metadata)} if item.metadata else {}),
            }
            for item in timeline.transitions
        ],
        "markers": [
            {
                "id": marker.id,
                **({"track": marker.track} if marker.track is not None else {}),
                "start": _time_value(marker.start_ns, 2),
                "duration": _time_value(marker.duration_ns, 2),
                "name": marker.name,
                **({"metadata": dict(marker.metadata)} if marker.metadata else {}),
            }
            for marker in timeline.markers
        ],
    }
    if timeline.metadata:
        document["metadata"] = dict(timeline.metadata)
    return document


def downgrade_v2_document(document: dict[str, Any], *,
                          source: str = "<memory>") -> tuple[dict[str, Any], list[str]]:
    """Convert a v2 envelope to v1, preserving clips and emitting warnings."""
    timeline = parse_timeline_document(document, source=source)
    return to_v1_document(timeline)


def upgrade_v1_document(document: dict[str, Any]) -> dict[str, Any]:
    """Canonical v1-to-v2 upgrade; v2 input is returned unchanged."""
    timeline = parse_timeline_document(document)
    if timeline.version == 1:
        timeline.version = 2
    return to_v2_document(timeline)


def write_tle(timeline: Timeline, path: str | Path, *, version: int | None = None) -> list[str]:
    target_version = timeline.version if version is None else version
    if target_version == 1:
        document, warnings = to_v1_document(timeline)
    elif target_version == 2:
        document = to_v2_document(timeline)
        warnings = []
    else:
        raise TLEError(f"unsupported output version: {target_version}", source=str(path))
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(
        json.dumps(document, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    return warnings


# Short aliases are handy for callers that model interchange as a verb.
downgrade = downgrade_v2_document
upgrade = upgrade_v1_document
load_tle = read_tle
save_tle = write_tle
downgrade_v2_to_v1 = downgrade_v2_document
upgrade_v1_to_v2 = upgrade_v1_document
parse_document = parse_timeline_document
