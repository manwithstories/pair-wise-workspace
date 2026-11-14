"""ETJ version conversion and OpenTimelineIO exchange."""

from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import opentimelineio as otio

from .etj_v1 import timeline_to_v1, timeline_from_v1, write_v1
from .etj_v2 import timeline_from_v2, timeline_to_v2, write_v2
from .io_utils import DocumentIOError, ExchangeError, load_document, save_document
from .model import Segment, Timeline, Track
from .timebase import SAMPLE_RATE, parse_rate


class FrameRateMismatchError(ExchangeError):
    """Raised when one OTIO track disagrees with the timeline rate."""


def read_etj(path: str | Path) -> Timeline:
    data = load_document(path)
    version = data.get("version")
    if data.get("format") != "etj":
        raise ExchangeError(f"{path}: format must be etj")
    if version == 1:
        return timeline_from_v1(data)
    if version == 2:
        return timeline_from_v2(data)
    raise ExchangeError(f"{path}: unsupported ETJ version: {version!r}")


def write_etj(timeline: Timeline, path: str | Path, version: int) -> list[str]:
    if version == 1:
        return write_v1(timeline, path)
    if version == 2:
        write_v2(timeline, path)
        return []
    raise ExchangeError(f"unsupported ETJ version: {version}")


def convert_etj(
    source: str | Path, destination: str | Path, version: int
) -> tuple[Timeline, list[str]]:
    timeline = read_etj(source)
    warnings = write_etj(timeline, destination, version)
    return timeline, warnings


def _model_payload(timeline: Timeline) -> dict[str, Any]:
    return {
        "name": timeline.name,
        "fps": str(timeline.fps),
        "declared_duration": timeline.declared_duration,
        "metadata": timeline.metadata,
        "extensions": timeline.extensions,
        "locked": timeline.locked,
        "tracks": [
            {
                "id": track.id,
                "kind": track.kind,
                "z_order": track.z_order,
                "priority": track.priority,
                "locked": track.locked,
                "metadata": track.metadata,
                "extensions": track.extensions,
                "segments": [
                    {
                        "id": segment.id,
                        "start": segment.start,
                        "duration": segment.duration,
                        "source_start": segment.source_start,
                        "priority": segment.priority,
                        "locked": segment.locked,
                        "metadata": segment.metadata,
                        "extensions": segment.extensions,
                    }
                    for segment in sorted(track.segments, key=lambda item: (item.start, item.id))
                ],
            }
            for track in timeline.sorted_tracks()
        ],
    }


def model_summary(timeline: Timeline) -> dict[str, Any]:
    payload = _model_payload(timeline)
    canonical = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return {
        "name": timeline.name,
        "fps": str(timeline.fps),
        "track_count": len(timeline.tracks),
        "segment_count": timeline.segment_count(),
        "duration_samples": timeline.duration(),
        "digest": hashlib.sha256(canonical.encode("utf-8")).hexdigest(),
    }


def to_otio(timeline: Timeline) -> Any:
    value = otio.schema.Timeline(name=timeline.name)
    value.global_start_time = otio.opentime.RationalTime(0, SAMPLE_RATE)
    value.metadata["tlcore"] = {
        "name": timeline.name,
        "fps": str(timeline.fps),
        "declared_duration": timeline.declared_duration,
        "metadata": dict(timeline.metadata),
        "extensions": dict(timeline.extensions),
        "locked": timeline.locked,
    }
    for track in timeline.tracks:
        lanes = _assign_lanes(track)
        for lane_index, lane in enumerate(lanes):
            otio_track = otio.schema.Track(
                name=f"{track.id}.lane{lane_index}",
                kind="Audio" if track.kind == "audio" else "Video",
            )
            otio_track.metadata["tlcore"] = {
                "id": track.id,
                "kind": track.kind,
                "z_order": track.z_order,
                "priority": track.priority,
                "locked": track.locked,
                "metadata": dict(track.metadata),
                "extensions": dict(track.extensions),
                "fps": str(timeline.fps),
                "lane": lane_index,
                "lane_count": len(lanes),
            }
            cursor = 0
            for segment in lane:
                if segment.start > cursor:
                    otio_track.append(
                        otio.schema.Gap(duration=_range(segment.start - cursor).duration)
                    )
                clip = otio.schema.Clip(
                    name=segment.id,
                    source_range=_range(segment.duration, segment.source_start),
                )
                clip.metadata["tlcore"] = {
                    "id": segment.id,
                    "priority": segment.priority,
                    "locked": segment.locked,
                    "metadata": dict(segment.metadata),
                    "extensions": dict(segment.extensions),
                    "source_start": segment.source_start,
                    "start_sample": segment.start,
                }
                otio_track.append(clip)
                cursor = segment.end
            value.tracks.append(otio_track)
    return value


def from_otio(value: Any) -> Timeline:
    if not isinstance(value, otio.schema.Timeline):
        raise ExchangeError("OTIO root must be a Timeline")
    timeline_meta = _meta(value.metadata, "timeline")
    timeline_fps = parse_rate(timeline_meta.get("fps", 24))
    timeline = Timeline(
        name=str(timeline_meta.get("name", value.name or "timeline")),
        fps=timeline_fps,
        metadata=dict(timeline_meta.get("metadata", {})),
        extensions=dict(timeline_meta.get("extensions", {})),
        locked=bool(timeline_meta.get("locked", False)),
        declared_duration=timeline_meta.get("declared_duration"),
    )
    tracks_by_id: dict[str, Track] = {}
    for track_index, otio_track in enumerate(value.tracks):
        track_meta = _meta(otio_track.metadata, f"track {otio_track.name}")
        track_id = str(track_meta.get("id", otio_track.name or f"track{track_index}"))
        track_fps = parse_rate(track_meta.get("fps", timeline_fps))
        if track_fps != timeline_fps:
            raise FrameRateMismatchError(
                f"track {track_id}: frame rate {track_fps} differs from timeline {timeline_fps}"
            )
        kind = str(track_meta.get("kind", "audio" if otio_track.kind == "Audio" else "video"))
        lane_group = "lane" in track_meta and "lane_count" in track_meta
        if track_id in tracks_by_id and lane_group:
            track = tracks_by_id[track_id]
            if track.kind != kind:
                raise ExchangeError(f"track {track_id}: inconsistent kind across lanes")
        else:
            track = Track(
                id=track_id,
                kind=kind,
                z_order=int(track_meta.get("z_order", track_index)),
                priority=int(track_meta.get("priority", 0)),
                locked=bool(track_meta.get("locked", False)),
                metadata=dict(track_meta.get("metadata", {})),
                extensions=dict(track_meta.get("extensions", {})),
            )
            tracks_by_id[track_id] = track
            timeline.tracks.append(track)
        cursor = 0
        item_rate: float | None = None
        for otio_item in otio_track:
            duration = _samples_from_rational(otio_item.duration())
            if isinstance(otio_item, otio.schema.Clip):
                source_range = otio_item.source_range
                if source_range is None:
                    raise ExchangeError(f"track {track_id}: clip {otio_item.name} has no source_range")
                if not _rate_matches(source_range.start_time.rate, track_fps) or not (
                    _rate_matches(source_range.duration.rate, track_fps)
                ):
                    raise FrameRateMismatchError(
                        f"track {track_id}: clip {otio_item.name} has an inconsistent frame rate"
                    )
                if item_rate is None:
                    item_rate = float(source_range.duration.rate)
                elif math.isclose(item_rate, float(source_range.duration.rate)) is False:
                    raise FrameRateMismatchError(f"track {track_id}: inconsistent clip time rates")
                clip_meta = _meta(otio_item.metadata, f"track {track_id} clip {otio_item.name}")
                start = int(clip_meta.get("start_sample", cursor))
                track.segments.append(
                    Segment(
                        id=str(clip_meta.get("id", otio_item.name or f"clip{len(track.segments)}")),
                        start=start,
                        duration=duration,
                        source_start=_samples_from_rational(source_range.start_time),
                        priority=int(clip_meta.get("priority", track.priority)),
                        locked=bool(clip_meta.get("locked", track.locked)),
                        metadata=dict(clip_meta.get("metadata", {})),
                        extensions=dict(clip_meta.get("extensions", {})),
                    )
                )
            cursor += duration
    for track in timeline.tracks:
        track.segments.sort(key=lambda item: (item.start, item.id))
    return timeline


def write_otio(timeline: Timeline, path: str | Path) -> None:
    value = to_otio(timeline)
    try:
        location = Path(path)
        location.parent.mkdir(parents=True, exist_ok=True)
        otio.adapters.write_to_file(value, str(location))
    except (OSError, ValueError, otio.exceptions.OTIOError) as exc:
        raise DocumentIOError(f"cannot export OTIO {path}: {exc}") from exc


def read_otio(path: str | Path) -> Timeline:
    try:
        value = otio.adapters.read_from_file(str(path))
    except (OSError, ValueError, otio.exceptions.OTIOError) as exc:
        raise DocumentIOError(f"cannot import OTIO {path}: {exc}") from exc
    return from_otio(value)


def _range(duration: int, start: int = 0) -> Any:
    return otio.opentime.TimeRange(
        otio.opentime.RationalTime(start, SAMPLE_RATE),
        otio.opentime.RationalTime(duration, SAMPLE_RATE),
    )


def _samples_from_rational(value: Any) -> int:
    return int(round(float(value.value) * SAMPLE_RATE / float(value.rate)))


def _meta(value: Any, label: str) -> dict[str, Any]:
    nested = value.get("tlcore", {}) if hasattr(value, "get") else {}
    if not isinstance(nested, dict):
        try:
            nested = dict(nested)
        except (TypeError, ValueError) as exc:
            raise ExchangeError(f"{label}: tlcore metadata must be a mapping") from exc
    return nested


def _rate_matches(value: Any, track_fps: Any) -> bool:
    rate = float(value)
    return math.isclose(rate, float(SAMPLE_RATE)) or math.isclose(
        rate, float(track_fps), rel_tol=1e-9
    )


def _assign_lanes(track: Track) -> list[list[Segment]]:
    lanes: list[list[Segment]] = []
    lane_ends: list[int] = []
    for segment in track.sorted_segments():
        for index, end in enumerate(lane_ends):
            if end <= segment.start:
                lanes[index].append(segment)
                lane_ends[index] = segment.end
                break
        else:
            lanes.append([segment])
            lane_ends.append(segment.end)
    return lanes or [[]]
