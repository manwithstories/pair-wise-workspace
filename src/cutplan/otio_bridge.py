from __future__ import annotations

from copy import deepcopy
from collections.abc import Mapping
from pathlib import Path
from typing import Any

import opentimelineio as otio

from .errors import CutplanError
from .model import Clip, Manifest, Timeline, Track
from .timebase import FrameRate, parse_rate


_NS = "cutplan"


def _plain(value: Any) -> Any:
    if isinstance(value, Mapping):
        return {str(key): _plain(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_plain(item) for item in value]
    return value


def _clip_list(timeline: Timeline) -> list[tuple[Track, Clip]]:
    rows = [(track, clip) for track in timeline.tracks for clip in track.clips]
    return sorted(rows, key=lambda row: (row[0].id, row[1].start, row[1].id))


def to_otio(timeline: Timeline) -> otio.schema.Timeline:
    rate = float(timeline.rate.fps)
    result = otio.schema.Timeline(name=timeline.manifest.title or "cutplan")
    result.metadata[_NS] = {
        "model": 1,
        "manifest": {
            "title": timeline.manifest.title,
            "schema": timeline.manifest.schema,
            "ext": deepcopy(timeline.manifest.ext),
            "metadata": deepcopy(timeline.manifest.metadata),
        },
        "rate": str(timeline.rate),
        "declared_duration": timeline.declared_duration,
        "ext": deepcopy(timeline.ext),
        "metadata": deepcopy(timeline.metadata),
    }
    stack = otio.schema.Stack(name="tracks")
    result.tracks = stack
    for track in timeline.tracks:
        otio_kind = "Audio" if track.kind.strip().lower() in {"audio", "music", "voice", "音轨"} else "Video"
        otio_track = otio.schema.Track(name=track.id, kind=otio_kind)
        otio_track.metadata[_NS] = {
            "kind": track.kind,
            "priority": track.priority,
            "ext": deepcopy(track.ext),
            "metadata": deepcopy(track.metadata),
        }
        cursor = 0
        for clip in sorted(track.clips, key=lambda item: (item.start, item.id)):
            if clip.duration <= 0:
                raise CutplanError("E_OTIO_ZERO_CLIP", f"cannot export zero-duration clip: {clip.id}", exit_code=5)
            if clip.start < cursor:
                raise CutplanError(
                    "E_OTIO_OVERLAP",
                    f"track {track.id} has overlapping clips; resolve before OTIO export",
                    exit_code=4,
                )
            if clip.start > cursor:
                otio_track.append(_gap(clip.start - cursor, rate))
            otio_track.append(_to_otio_clip(clip, rate))
            cursor = clip.end
        target = timeline.output_duration_frames
        if cursor < target:
            otio_track.append(_gap(target - cursor, rate))
        stack.append(otio_track)
    return result


def _gap(duration: int, rate: float) -> otio.schema.Gap:
    return otio.schema.Gap(
        source_range=otio.opentime.TimeRange(
            otio.opentime.RationalTime(0, rate),
            otio.opentime.RationalTime(duration, rate),
        )
    )


def _to_otio_clip(clip: Clip, rate: float) -> otio.schema.Clip:
    result = otio.schema.Clip(name=clip.id)
    result.source_range = otio.opentime.TimeRange(
        otio.opentime.RationalTime(clip.source_start, rate),
        otio.opentime.RationalTime(clip.duration, rate),
    )
    result.metadata[_NS] = {
        "id": clip.id,
        "priority": clip.priority,
        "role": clip.role,
        "media_ref": clip.media_ref,
        "ext": deepcopy(clip.ext),
    }
    return result


def from_otio(source: otio.schema.Timeline) -> Timeline:
    if not isinstance(source, otio.schema.Timeline):
        raise CutplanError("E_OTIO_TYPE", "expected an OTIO Timeline")
    root_meta = _plain(source.metadata.get(_NS, {}))
    if not isinstance(root_meta, dict):
        root_meta = {}
    rate = _rate_from(root_meta, source)
    manifest_meta = root_meta.get("manifest", {})
    if not isinstance(manifest_meta, dict):
        manifest_meta = {}
    manifest = Manifest(
        title=str(manifest_meta.get("title", source.name or "")),
        schema=str(manifest_meta.get("schema", "cutplan/v2")),
        ext=_dict(manifest_meta.get("ext")),
        metadata=_dict(manifest_meta.get("metadata")),
    )
    tracks: list[Track] = []
    for child in source.tracks:
        if isinstance(child, otio.schema.Stack):
            continue
        if not isinstance(child, otio.schema.Track):
            raise CutplanError("E_OTIO_CHILD", f"unsupported stack child: {type(child).__name__}", exit_code=5)
        tracks.append(_track_from(child, rate))
    declared = root_meta.get("declared_duration")
    if not isinstance(declared, int) or isinstance(declared, bool):
        declared = int(round(source.tracks.duration().value)) if source.tracks.duration() else 0
    timeline = Timeline(
        rate=rate,
        declared_duration=max(0, declared),
        tracks=tracks,
        manifest=manifest,
        ext=_dict(root_meta.get("ext")),
        metadata=_dict(root_meta.get("metadata")),
        source_format="otio",
    )
    return timeline


def _track_from(source: otio.schema.Track, fallback_rate: FrameRate) -> Track:
    meta = _plain(source.metadata.get(_NS, {}))
    if not isinstance(meta, dict):
        meta = {}
    kind = meta.get("kind")
    if not isinstance(kind, str):
        kind = "audio" if str(source.kind).lower() == "audio" else "main"
    priority = meta.get("priority", 0)
    if not isinstance(priority, int) or isinstance(priority, bool):
        priority = 0
    clips: list[Clip] = []
    cursor = 0
    for child in source:
        if isinstance(child, otio.schema.Gap):
            cursor += _frames(child.duration(), fallback_rate)
        elif isinstance(child, otio.schema.Clip):
            clip_meta = _plain(child.metadata.get(_NS, {}))
            if not isinstance(clip_meta, dict):
                clip_meta = {}
            source_range = child.source_range
            if source_range is None:
                raise CutplanError("E_OTIO_CLIP_RANGE", f"clip has no source_range: {child.name}", exit_code=5)
            duration = _frames(source_range.duration, fallback_rate)
            source_start = _frames(source_range.start_time, fallback_rate)
            clip_id = clip_meta.get("id", child.name)
            if not isinstance(clip_id, str) or not clip_id:
                raise CutplanError("E_OTIO_CLIP_ID", "clip has no usable id", exit_code=5)
            role = clip_meta.get("role", "")
            media_ref = clip_meta.get("media_ref", "")
            priority_value = clip_meta.get("priority", 0)
            clips.append(
                Clip(
                    id=clip_id,
                    start=cursor,
                    duration=duration,
                    source_start=source_start,
                    priority=priority_value if isinstance(priority_value, int) else 0,
                    role=role if isinstance(role, str) else "",
                    media_ref=media_ref if isinstance(media_ref, str) else "",
                    ext=_dict(clip_meta.get("ext")),
                )
            )
            cursor += duration
        else:
            raise CutplanError("E_OTIO_CHILD", f"unsupported track child: {type(child).__name__}", exit_code=5)
    return Track(
        id=source.name or f"track-{len(clips)}",
        kind=kind,
        clips=clips,
        priority=priority,
        ext=_dict(meta.get("ext")),
        metadata=_dict(meta.get("metadata")),
    )


def _rate_from(meta: dict[str, Any], source: otio.schema.Timeline) -> FrameRate:
    value = meta.get("rate")
    if isinstance(value, str):
        return parse_rate(value)
    if source.global_start_time is not None:
        return parse_rate_from_fps(source.global_start_time.rate)
    for track in source.tracks:
        if hasattr(track, "duration"):
            duration = track.duration()
            if duration is not None and duration.rate:
                return parse_rate_from_fps(duration.rate)
    return parse_rate("24")


def parse_rate_from_fps(value: float) -> FrameRate:
    if abs(value - 30000 / 1001) < 0.001:
        return parse_rate("30000/1001")
    rounded = int(round(value))
    if abs(value - rounded) > 0.001:
        raise CutplanError("E_OTIO_RATE", f"unsupported OTIO rate {value!r}", exit_code=5)
    return parse_rate(rounded)


def _frames(time: otio.opentime.RationalTime, rate: FrameRate) -> int:
    if abs(time.rate - float(rate.fps)) > 0.001:
        seconds = time.to_seconds()
        return int(round(seconds * float(rate.fps)))
    return int(round(time.value))


def _dict(value: Any) -> dict[str, Any]:
    if not isinstance(value, Mapping):
        return {}
    return {str(key): deepcopy(_plain(item)) for key, item in value.items()}


def export_timeline(timeline: Timeline, path: str | Path) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    try:
        otio.adapters.write_to_file(to_otio(timeline), str(destination))
    except CutplanError:
        raise
    except Exception as exc:
        raise CutplanError("E_OTIO_WRITE", "failed to write OTIO file", exit_code=5) from exc


def import_timeline(path: str | Path) -> Timeline:
    try:
        value = otio.adapters.read_from_file(str(path))
    except Exception as exc:
        raise CutplanError("E_OTIO_READ", "failed to read OTIO file", exit_code=5) from exc
    return from_otio(value)


def roundtrip_check(path: str | Path) -> dict[str, Any]:
    first = import_timeline(path)
    second = from_otio(to_otio(first))
    equal = first.canonical_dict() == second.canonical_dict()
    delta = abs(first.duration_frames - second.duration_frames)
    return {
        "ok": equal and delta == 0,
        "equal": equal,
        "duration_delta_frames": delta,
        "duration_delta_seconds": 0.0 if delta == 0 else delta / float(first.rate.fps),
        "policy": first.metadata.get("resolution_policy", ""),
        "tracks": len(first.tracks),
        "clips": first.clip_count,
    }
