"""Bidirectional mapping between the internal model and OpenTimelineIO."""

from __future__ import annotations

from fractions import Fraction
from pathlib import Path
from typing import Any

import opentimelineio as otio

from .model import Clip, LinkGroup, Marker, ModelError, Timeline, Track, Transition
from .timebase import NS_PER_SECOND


class OTIOBridgeError(ValueError):
    pass


def _rt(ns: int) -> otio.opentime.RationalTime:
    return otio.opentime.RationalTime(ns, NS_PER_SECOND)


def _ns(value: otio.opentime.RationalTime) -> int:
    rescaled = value.rescaled_to(NS_PER_SECOND)
    exact = Fraction(str(rescaled.value))
    if exact.denominator != 1:
        raise OTIOBridgeError(f"OTIO time is not an integral nanosecond: {rescaled.value}")
    return exact.numerator


def _track_kind(kind: str) -> str:
    return "Audio" if kind in ("audio", "music") else "Video"


def _model_kind(kind: str) -> str:
    if kind == "Audio":
        return "audio"
    if kind == "Video":
        return "video"
    return "subtitle" if "subtitle" in kind.lower() else kind.lower()


def _clip_metadata(clip: Clip) -> dict[str, Any]:
    return {
        "id": clip.id,
        "track": clip.track,
        "kind": clip.kind,
        "start_ns": clip.start_ns,
        "duration_ns": clip.duration_ns,
        "source_in_ns": clip.source_in_ns,
        "source_duration_ns": clip.source_duration_ns,
        "link_id": clip.link_id,
        "priority": clip.priority,
        "metadata": dict(clip.metadata),
    }


def to_otio(timeline: Timeline) -> otio.schema.Timeline:
    """Build an OTIO timeline; exact internal values live in ``metadata.tlcore``."""
    result = otio.schema.Timeline(name=str(timeline.metadata.get("name", "show")))
    result.metadata["tlcore"] = {
        "version": timeline.version,
        "fps_num": timeline.fps.numerator,
        "fps_den": timeline.fps.denominator,
        "declared_duration_ns": timeline.declared_duration_ns,
        "metadata": dict(timeline.metadata),
        "links": [
            {"id": group.id, "clips": list(group.clip_ids)}
            for group in timeline.link_groups
        ],
        "markers": [
            {
                "id": marker.id,
                "track": marker.track,
                "start_ns": marker.start_ns,
                "duration_ns": marker.duration_ns,
                "name": marker.name,
                "metadata": dict(marker.metadata),
            }
            for marker in timeline.markers
        ],
        "transitions": [
            {
                "id": item.id,
                "track": item.track,
                "from_clip_id": item.from_clip_id,
                "to_clip_id": item.to_clip_id,
                "duration_ns": item.duration_ns,
                "max_duration_ns": item.max_duration_ns,
                "metadata": dict(item.metadata),
            }
            for item in timeline.transitions
        ],
    }
    transitions_by_from = {
        (item.track, item.from_clip_id): item for item in timeline.transitions
    }
    for model_track in timeline.tracks:
        track = otio.schema.Track(
            name=model_track.id,
            kind=_track_kind(model_track.kind),
            metadata={"tlcore": {"id": model_track.id, "kind": model_track.kind,
                                 "metadata": dict(model_track.metadata)}},
        )
        cursor_ns = 0
        previous_clip: Clip | None = None
        for model_clip in model_track.ordered_clips():
            gap_ns = model_clip.start_ns - cursor_ns
            if gap_ns > 0:
                track.append(otio.schema.Gap(
                    source_range=otio.opentime.TimeRange(
                        duration=_rt(gap_ns), start_time=_rt(0)
                    )
                ))
                cursor_ns = model_clip.start_ns
            transition = transitions_by_from.get((model_clip.track, previous_clip.id)) if previous_clip else None
            if transition is not None and gap_ns <= 0:
                track.append(otio.schema.Transition(
                    name=transition.id,
                    transition_type=otio.schema.TransitionTypes.SMPTE_Dissolve,
                    in_offset=_rt(transition.duration_ns),
                    out_offset=_rt(transition.duration_ns),
                    metadata={"tlcore": {
                        "id": transition.id, "track": transition.track,
                        "from_clip_id": transition.from_clip_id,
                        "to_clip_id": transition.to_clip_id,
                        "duration_ns": transition.duration_ns,
                        "max_duration_ns": transition.max_duration_ns,
                    }},
                ))
            clip = otio.schema.Clip(
                name=model_clip.id,
                source_range=otio.opentime.TimeRange(
                    start_time=_rt(model_clip.source_in_ns),
                    duration=_rt(model_clip.duration_ns),
                ),
                metadata={"tlcore": _clip_metadata(model_clip)},
            )
            if model_clip.source_duration_ns is not None:
                clip.media_reference = otio.schema.MissingReference(
                    metadata={"tlcore": {
                        "source_duration_ns": model_clip.source_duration_ns
                    }}
                )
            else:
                clip.media_reference = otio.schema.MissingReference()
            track.append(clip)
            cursor_ns += model_clip.duration_ns
            previous_clip = model_clip
        for marker_model in timeline.markers:
            if marker_model.track == model_track.id:
                track.markers.append(otio.schema.Marker(
                    name=marker_model.name or marker_model.id,
                    marked_range=otio.opentime.TimeRange(
                        start_time=_rt(marker_model.start_ns),
                        duration=_rt(marker_model.duration_ns),
                    ),
                    metadata={"tlcore": {
                        "id": marker_model.id, "duration_ns": marker_model.duration_ns,
                        "metadata": dict(marker_model.metadata),
                    }},
                ))
        if timeline.declared_duration_ns > 0:
            track.source_range = otio.opentime.TimeRange(
                start_time=_rt(0), duration=_rt(timeline.declared_duration_ns)
            )
        result.tracks.append(track)
    if timeline.declared_duration_ns > 0 and len(result.tracks) == 0:
        result.tracks.append(otio.schema.Track(
            name="__duration__", kind=otio.schema.TrackKind.Video,
            children=[otio.schema.Gap(source_range=otio.opentime.TimeRange(
                start_time=_rt(0), duration=_rt(timeline.declared_duration_ns)
            ))],
            metadata={"tlcore": {"internal": False}},
        ))
    return result


def _clip_from_item(item: Any, track_id: str, track_kind: str, cursor_ns: int) -> Clip:
    metadata = item.metadata.get("tlcore")
    if metadata:
        try:
            return Clip(
                id=str(metadata.get("id", item.name)),
                track=str(metadata.get("track", track_id)),
                kind=str(metadata.get("kind", track_kind)),
                start_ns=int(metadata["start_ns"]),
                duration_ns=int(metadata["duration_ns"]),
                source_in_ns=int(metadata.get("source_in_ns", 0)),
                source_duration_ns=(
                    int(metadata["source_duration_ns"])
                    if metadata.get("source_duration_ns") is not None else None
                ),
                link_id=metadata.get("link_id"),
                priority=int(metadata.get("priority", 0)),
                metadata=dict(metadata.get("metadata", {})),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise OTIOBridgeError(f"invalid tlcore metadata on clip {item.name!r}") from exc
    if not isinstance(item, otio.schema.Clip):
        raise OTIOBridgeError(f"expected Clip, got {type(item).__name__}")
    source = item.source_range
    if source is None:
        raise OTIOBridgeError(f"clip {item.name!r} has no source_range")
    available = None
    media_metadata = item.media_reference.metadata.get("tlcore", {}) if item.media_reference else {}
    if media_metadata.get("source_duration_ns") is not None:
        available = int(media_metadata["source_duration_ns"])
    return Clip(
        id=str(item.name),
        track=track_id,
        kind=track_kind,
        start_ns=cursor_ns,
        duration_ns=_ns(source.duration),
        source_in_ns=_ns(source.start_time),
        source_duration_ns=available,
    )


def from_otio(otio_timeline: Any) -> Timeline:
    if not isinstance(otio_timeline, otio.schema.Timeline):
        raise OTIOBridgeError("expected an OpenTimelineIO Timeline")
    top = otio_timeline.metadata.get("tlcore", {})
    fps = Fraction(int(top.get("fps_num", 25)), int(top.get("fps_den", 1)))
    has_tlcore_metadata = bool(top)
    tracks: list[Track] = []
    inferred_transitions: list[Transition] = []
    inferred_markers: list[Marker] = []
    for otio_track in otio_timeline.tracks:
        track_meta = otio_track.metadata.get("tlcore", {})
        if track_meta.get("internal") is False:
            continue
        track_id = str(track_meta.get("id", otio_track.name))
        track_kind = str(track_meta.get("kind", _model_kind(str(otio_track.kind))))
        clips: list[Clip] = []
        cursor_ns = 0
        items = list(otio_track)
        for item_index, item in enumerate(items):
            if isinstance(item, otio.schema.Gap):
                if item.source_range is not None:
                    cursor_ns += _ns(item.source_range.duration)
            elif isinstance(item, otio.schema.Clip):
                clip = _clip_from_item(item, track_id, track_kind, cursor_ns)
                clips.append(clip)
                if not item.metadata.get("tlcore"):
                    cursor_ns += clip.duration_ns
                else:
                    cursor_ns = max(cursor_ns + clip.duration_ns, clip.end_ns)
            elif isinstance(item, otio.schema.Transition) and not has_tlcore_metadata:
                previous_id = next(
                    (items[index].name for index in range(item_index - 1, -1, -1)
                     if isinstance(items[index], otio.schema.Clip)), None
                )
                next_id = next(
                    (items[index].name for index in range(item_index + 1, len(items))
                     if isinstance(items[index], otio.schema.Clip)), None
                )
                if previous_id and next_id:
                    in_offset = _ns(item.in_offset) if item.in_offset is not None else 0
                    out_offset = _ns(item.out_offset) if item.out_offset is not None else 0
                    inferred_transitions.append(Transition(
                        id=str(item.name or f"transition.{len(inferred_transitions)}"),
                        track=track_id, from_clip_id=str(previous_id),
                        to_clip_id=str(next_id), duration_ns=in_offset + out_offset,
                    ))
        tracks.append(Track(
            id=track_id,
            kind=track_kind,
            clips=clips,
            metadata=dict(track_meta.get("metadata", {})),
        ))
        if not has_tlcore_metadata:
            for marker_item in otio_track.markers:
                marked = marker_item.marked_range
                inferred_markers.append(Marker(
                    id=str(marker_item.metadata.get("tlcore", {}).get(
                        "id", marker_item.name or f"marker.{len(inferred_markers)}"
                    )),
                    track=track_id,
                    start_ns=_ns(marked.start_time) if marked is not None else 0,
                    duration_ns=_ns(marked.duration) if marked is not None else 0,
                    name=str(marker_item.name or ""),
                ))
    transitions: list[Transition] = []
    for raw in top.get("transitions", []):
        transitions.append(Transition(
            id=str(raw["id"]), track=str(raw["track"]),
            from_clip_id=str(raw["from_clip_id"]), to_clip_id=str(raw["to_clip_id"]),
            duration_ns=int(raw["duration_ns"]),
            max_duration_ns=(
                int(raw["max_duration_ns"])
                if raw.get("max_duration_ns") is not None else None
            ),
            metadata=dict(raw.get("metadata", {})),
        ))
    markers = [Marker(
        id=str(raw["id"]), track=raw.get("track"), start_ns=int(raw["start_ns"]),
        duration_ns=int(raw.get("duration_ns", 0)), name=str(raw.get("name", "")),
        metadata=dict(raw.get("metadata", {})),
    ) for raw in top.get("markers", [])] + inferred_markers
    groups = [LinkGroup(str(raw["id"]), [str(cid) for cid in raw["clips"]])
              for raw in top.get("links", [])]
    declared = int(top.get("declared_duration_ns", 0))
    if not top:
        declared = _ns(otio_timeline.duration())
    timeline = Timeline(
        version=int(top.get("version", 1)), fps=fps, declared_duration_ns=declared,
        tracks=tracks, transitions=transitions + inferred_transitions, link_groups=groups,
        markers=markers, metadata=dict(top.get("metadata", {})),
    )
    try:
        timeline.validate()
    except ModelError as exc:
        raise OTIOBridgeError(f"OTIO mapping produced an invalid timeline: {exc}") from exc
    return timeline


def write_otio(timeline: Timeline, path: str | Path) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    otio.adapters.write_to_file(to_otio(timeline), str(destination))


def read_otio(path: str | Path) -> Timeline:
    try:
        loaded = otio.adapters.read_from_file(str(path))
    except Exception as exc:
        raise OTIOBridgeError(f"cannot read OTIO file {path}: {exc}") from exc
    return from_otio(loaded)


def otio_duration_ns(otio_timeline: Any) -> int:
    return _ns(otio_timeline.duration())


def duration_report(timeline: Timeline, otio_timeline: Any | None = None,
                    *, path: str | Path | None = None) -> dict[str, Any]:
    if otio_timeline is None:
        if path is not None:
            try:
                otio_timeline = otio.adapters.read_from_file(str(path))
            except Exception as exc:
                raise OTIOBridgeError(f"cannot read OTIO file {path}: {exc}") from exc
        else:
            otio_timeline = to_otio(timeline)
    if isinstance(otio_timeline, Timeline):
        otio_ns = otio_timeline.computed_duration_ns
    else:
        otio_ns = otio_duration_ns(otio_timeline)
    return {
        "duration_declared_ns": timeline.declared_duration_ns,
        "duration_computed_ns": timeline.computed_duration_ns,
        "otio_duration_ns": otio_ns,
        "deviation_ns": timeline.declared_duration_ns - otio_ns,
    }


to_timeline = from_otio
