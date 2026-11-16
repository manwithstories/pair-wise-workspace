"""`inspect` payload assembly: element tree, tracks, timing and mapping."""

from __future__ import annotations

from .matroska_model import MatroskaDocument
from .timestamp import TimestampError, plan_rewrite
from .track_mapper import TrackMappingError, build_track_mapping


def _element_tree(document: MatroskaDocument) -> list[dict[str, object]]:
    """A compact structural outline for ``inspect`` (no payload bytes)."""
    rows: list[dict[str, object]] = []
    for cluster in document.clusters:
        rows.append(
            {
                "name": "Cluster",
                "offset": cluster.offset,
                "size": cluster.size,
                "unknown_size": cluster.unknown_size,
                "timecode": cluster.timecode,
                "blocks": len(cluster.blocks),
            }
        )
    return rows


def inspect_document(document: MatroskaDocument, warnings: list[dict]) -> dict[str, object]:
    plan = None
    plan_error = None
    try:
        plan = plan_rewrite(document)
    except TimestampError as exc:
        plan_error = exc.as_dict()
    mapping = None
    mapping_error = None
    try:
        mapping = build_track_mapping(document)
    except TrackMappingError as exc:
        mapping_error = exc.as_dict()
    payload: dict[str, object] = {
        "summary": document.summary(),
        "info": {
            "timecode_scale_ns": document.info.timestamp_scale_ns,
            "duration_ticks": document.info.duration_ticks,
            "duration_ns": document.info.duration_ns,
            "title": document.info.title,
            "muxing_app": document.info.muxing_app,
            "writing_app": document.info.writing_app,
            "segment_uid": document.info.segment_uid.hex() if document.info.segment_uid else None,
        },
        "tracks": [track.as_dict() for track in document.tracks],
        "clusters": _element_tree(document),
        "cues": [cue.as_dict() for cue in document.cues],
        "attachments": [
            {
                "uid": a.uid,
                "name": a.name,
                "mime_type": a.mime_type,
                "data_size": a.data_size,
            }
            for a in document.attachments
        ],
        "warnings": list(warnings) + list(document.warnings),
    }
    if plan is not None:
        payload["timestamp_plan"] = plan.summary()
    else:
        payload["timestamp_plan"] = None
        payload["timestamp_error"] = plan_error
    if mapping is not None:
        payload["track_mapping"] = mapping.summary()
    else:
        payload["track_mapping"] = None
        payload["track_mapping_error"] = mapping_error
    return payload


__all__ = ["inspect_document"]
