"""Assemble parsed EBML elements into a :class:`MatroskaDocument`."""

from __future__ import annotations

from pathlib import Path

from . import ebml_schema as ids
from ._matroska_block import build_cluster
from ._matroska_track import (
    _float_child,
    _str_child,
    _uint_child,
    build_track,
    decode_ascii_safe,
)
from ._matroska_types import (
    DEFAULT_TIMESTAMP_SCALE,
    Attachment,
    CuePoint,
    MatroskaDocument,
    ModelError,
    SegmentInfo,
)
from .ebml_reader import Element

def build_info(segment: Element, warnings: list[dict[str, object]]) -> SegmentInfo:
    info_el = segment.child(ids.INFO)
    if info_el is None:
        warnings.append(
            {"code": "info_missing", "message": "Segment has no Info; defaulting TimecodeScale", "offset": segment.offset}
        )
        return SegmentInfo()
    scale = _uint_child(info_el, ids.TIMESTAMP_SCALE, DEFAULT_TIMESTAMP_SCALE)
    if scale is None or scale == 0:
        warnings.append(
            {
                "code": "timecode_scale_invalid",
                "message": "TimecodeScale is zero; using 1000000 ns",
                "offset": info_el.offset,
            }
        )
        scale = DEFAULT_TIMESTAMP_SCALE
    uid_el = info_el.child(ids.SEGMENT_UID)
    return SegmentInfo(
        timestamp_scale_ns=scale,
        duration_ticks=_float_child(info_el, ids.DURATION),
        title=_str_child(info_el, ids.TITLE),
        muxing_app=_str_child(info_el, ids.MUXING_APP),
        writing_app=_str_child(info_el, ids.WRITING_APP),
        segment_uid=bytes(uid_el.data) if uid_el and uid_el.data else None,
    )


def build_cues(segment: Element, warnings: list[dict[str, object]]) -> list[CuePoint]:
    cues_el = segment.child(ids.CUES)
    if cues_el is None:
        return []
    out: list[CuePoint] = []
    for point in cues_el.children_named(ids.CUE_POINT):
        time = _uint_child(point, ids.CUE_TIME)
        if time is None:
            warnings.append(
                {"code": "cue_without_time", "message": "CuePoint lacks CueTime; skipped", "offset": point.offset}
            )
            continue
        positions = point.children_named(ids.CUE_TRACK_POSITIONS)
        if not positions:
            warnings.append(
                {
                    "code": "cue_without_track_position",
                    "message": "CuePoint lacks CueTrackPositions; skipped",
                    "offset": point.offset,
                }
            )
            continue
        for tp in positions:
            cluster_pos = _uint_child(tp, ids.CUE_CLUSTER_POSITION)
            if cluster_pos is None:
                warnings.append(
                    {
                        "code": "cue_without_cluster_position",
                        "message": "CueTrackPositions lacks CueClusterPosition; skipped",
                        "offset": tp.offset,
                    }
                )
                continue
            out.append(
                CuePoint(
                    time_ticks=time,
                    cluster_position=cluster_pos,
                    track_number=_uint_child(tp, ids.CUE_TRACK),
                    relative_position=_uint_child(tp, ids.CUE_RELATIVE_POSITION),
                    duration_ticks=_uint_child(tp, ids.CUE_DURATION),
                    offset=tp.offset,
                )
            )
    out.sort(key=lambda c: (c.time_ticks, c.cluster_position))
    return out


def build_attachments(segment: Element) -> list[Attachment]:
    container = segment.child(ids.ATTACHMENTS)
    if container is None:
        return []
    out = []
    for file_el in container.children_named(ids.ATTACHED_FILE):
        data_el = file_el.child(ids.FILE_DATA)
        out.append(
            Attachment(
                uid=_uint_child(file_el, ids.FILE_UID),
                name=_str_child(file_el, ids.FILE_NAME),
                mime_type=_str_child(file_el, ids.FILE_MIME_TYPE, decode_ascii_safe),
                description=_str_child(file_el, ids.FILE_DESCRIPTION),
                data_size=data_el.declared_size if data_el is not None else 0,
            )
        )
    return out


def build_document(
    root: Element,
    *,
    path: Path | None,
    file_size: int,
    element_count: int,
    unknown_length_count: int,
    warnings: list[dict[str, object]],
) -> MatroskaDocument:
    if root.child(ids.EBML_HEADER) is None:
        raise ModelError("ebml_header_missing", "document does not start with an EBML header", 0)
    segment = root.child(ids.SEGMENT)
    if segment is None:
        raise ModelError("segment_missing", "document has no Segment", 0)
    doc_warnings = list(warnings)
    info = build_info(segment, doc_warnings)
    tracks_el = segment.child(ids.TRACKS)
    tracks: list[Track] = []
    if tracks_el is None:
        raise ModelError("tracks_missing", "Segment has no Tracks element", segment.offset)
    for entry in tracks_el.children_named(ids.TRACK_ENTRY):
        tracks.append(build_track(entry, warnings=doc_warnings))
    if not tracks:
        raise ModelError("tracks_empty", "Tracks element has no TrackEntry", tracks_el.offset)
    seen: set[int] = set()
    for track in tracks:
        if track.number in seen:
            raise ModelError(
                "track_number_duplicate",
                f"track number {track.number} appears more than once",
                track.source_offset,
            )
        seen.add(track.number)

    clusters: list[Cluster] = []
    for index, cluster_el in enumerate(segment.children_named(ids.CLUSTER)):
        clusters.append(build_cluster(cluster_el, index=index, warnings=doc_warnings))
    cues = build_cues(segment, doc_warnings)
    attachments = build_attachments(segment)
    return MatroskaDocument(
        path=path,
        file_size=file_size,
        segment_offset=segment.offset,
        segment_data_offset=segment.data_offset,
        segment_size=None if segment.unknown_size else segment.resolved_size,
        segment_unknown_size=segment.unknown_size,
        info=info,
        tracks=tracks,
        clusters=clusters,
        cues=cues,
        attachments=attachments,
        element_count=element_count,
        unknown_length_count=unknown_length_count,
        warnings=doc_warnings,
    )


__all__ = [
    "Attachment",
    "AudioTrack",
    "Block",
    "Cluster",
    "CuePoint",
    "MatroskaDocument",
    "ModelError",
    "SegmentInfo",
    "Track",
    "VideoTrack",
    "build_cluster",
    "build_document",
    "parse_block_payload",
]
