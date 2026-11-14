"""Matroska track and block extraction built on the local EBML tree."""

from __future__ import annotations

from dataclasses import dataclass, field

from mkvtool.ebml.schema import (
    BLOCK_GROUP_ID,
    BLOCK_ID,
    CLUSTER_ID,
    CUES_ID,
    INFO_ID,
    SEGMENT_ID,
    SIMPLE_BLOCK_ID,
    TRACKS_ID,
)
from mkvtool.ebml.tree import Element, ParseResult, parse_ebml
from mkvtool.ebml.vint import read_vint
from mkvtool.errors import InputParseError

DEFAULT_TIMECODE_SCALE_NS = 1_000_000
SUPPORTED_VIDEO_CODECS = frozenset({"V_MPEG4/ISO/AVC"})
SUPPORTED_AUDIO_CODECS = frozenset({"A_AAC", "A_AAC/MPEG4/LC"})


@dataclass
class Track:
    number: int
    uid: int
    track_type: int
    codec_id: str
    codec_private: bytes = b""
    default_duration_ns: int | None = None
    language: str = "und"
    flag_default: bool = True
    pixel_width: int | None = None
    pixel_height: int | None = None
    sample_rate: float | None = None
    channels: int | None = None

    @property
    def kind(self) -> str:
        return {1: "video", 2: "audio"}.get(self.track_type, "other")

    @property
    def supported(self) -> bool:
        if self.track_type == 1:
            return self.codec_id in SUPPORTED_VIDEO_CODECS
        if self.track_type == 2:
            return self.codec_id in SUPPORTED_AUDIO_CODECS
        return False


@dataclass
class Sample:
    track_number: int
    timestamp_ticks: int
    duration_ticks: int | None
    keyframe: bool
    data: bytes
    cluster_index: int
    offset: int
    composition_offset_ticks: int = 0

    def timestamp_ns(self, timecode_scale_ns: int) -> int:
        return self.timestamp_ticks * timecode_scale_ns

@dataclass
class CueEntry:
    time_ticks: int
    track_number: int
    cluster_position: int
    relative_position: int | None
    block_number: int | None
    duration_ticks: int | None
    absolute_cluster_offset: int | None = None
    valid: bool = False
    reason: str = ""


@dataclass
class DemuxResult:
    parse: ParseResult
    segment: Element
    tracks: list[Track]
    samples: dict[int, list[Sample]]
    timecode_scale_ns: int
    duration_ns: int | None
    cues: list[CueEntry] = field(default_factory=list)
    cues_element_present: bool = False
    issues: list[dict] = field(default_factory=list)
    clusters: list[Element] = field(default_factory=list)

    @property
    def track_by_number(self) -> dict[int, Track]:
        return {track.number: track for track in self.tracks}


def _uint(element: Element | None, default: int | None = None) -> int | None:
    return default if element is None else int(element.value)


def _parse_tracks(tracks_element: Element) -> list[Track]:
    tracks: list[Track] = []
    for entry in tracks_element.children_of(0xAE):
        video = entry.child(0xE0)
        audio = entry.child(0xE1)
        language_ietf = entry.child_value(0x22B59D)
        language = language_ietf or entry.child_value(0x22B59C, "und") or "und"
        tracks.append(Track(
            number=int(entry.child_value(0xD7, len(tracks) + 1)),
            uid=int(entry.child_value(0x73C5, len(tracks) + 1)),
            track_type=int(entry.child_value(0x83, 0)),
            codec_id=str(entry.child_value(0x86, "")),
            codec_private=bytes(entry.child_value(0x63A2, b"") or b""),
            default_duration_ns=(
                int(entry.child_value(0x23E383))
                if entry.child(0x23E383) is not None else None
            ),
            language=str(language),
            flag_default=bool(entry.child_value(0x88, 1)),
            pixel_width=_uint(video.child(0xB0)) if video else None,
            pixel_height=_uint(video.child(0xBA)) if video else None,
            sample_rate=float(audio.child_value(0xB5)) if audio and audio.child(0xB5) else None,
            channels=_uint(audio.child(0x9F)) if audio else None,
        ))
    return tracks


def _block_payload(block_element: Element) -> tuple[int, int, int, bytes] | None:
    body = block_element.raw
    if len(body) < 4:
        return None
    track_number, width, _ = read_vint(body, 0)
    relative_offset = width
    if relative_offset + 3 > len(body):
        return None
    relative_ticks = int.from_bytes(body[relative_offset : relative_offset + 2], "big", signed=True)
    flags = body[relative_offset + 2]
    data_offset = relative_offset + 3
    if flags & 0x06:
        return None
    return track_number, relative_ticks, flags, body[data_offset:]


def _block_addition_offset(block_group: Element) -> int:
    additions = block_group.child(0x75A1)
    if additions is None:
        return 0
    for more in additions.children_of(0xA6):
        if int(more.child_value(0xEE, 1)) != 1:
            continue
        addition = more.child(0xA5)
        if addition is None:
            continue
        payload = bytes(addition.value)
        if payload.startswith(b"MKTB1"):
            try:
                return int.from_bytes(payload[5:9], "big", signed=True)
            except Exception:
                return 0
        if payload.startswith(b"PTSOFF:"):
            try:
                return int(payload[7:].decode("ascii").strip())
            except (ValueError, UnicodeDecodeError):
                return 0
    return 0


def _parse_cues(cues: Element, segment: Element, tracks: list[Track]) -> list[CueEntry]:
    segment_data_offset = segment.data_offset
    entries: list[CueEntry] = []
    known_tracks = {track.number for track in tracks}
    for point in cues.children_of(0xBB):
        cue_time = int(point.child_value(0xB3, 0))
        for positions in point.children_of(0xB7):
            track_number = int(positions.child_value(0xF7, 0))
            cluster_position = int(positions.child_value(0xF1, 0))
            entry = CueEntry(
                time_ticks=cue_time,
                track_number=track_number,
                cluster_position=cluster_position,
                relative_position=_uint(positions.child(0xF0)),
                block_number=_uint(positions.child(0x5378)),
                duration_ticks=_uint(positions.child(0xB2)),
                absolute_cluster_offset=segment_data_offset + cluster_position,
            )
            entry.valid = track_number in known_tracks
            entry.reason = "ok" if entry.valid else "unknown track"
            entries.append(entry)
    return entries


def _parse_document(parse_result: ParseResult) -> DemuxResult:
    segment = parse_result.first(SEGMENT_ID)
    if segment is None:
        raise InputParseError("Matroska Segment element is missing")
    info = segment.child(INFO_ID)
    timecode_scale_ns = int(info.child_value(0x2AD7B1, DEFAULT_TIMECODE_SCALE_NS)) if info else DEFAULT_TIMECODE_SCALE_NS
    if timecode_scale_ns <= 0:
        raise InputParseError("TimecodeScale must be positive")
    duration_ns = None
    if info is not None and info.child(0x4489) is not None:
        duration_ns = int(float(info.child_value(0x4489)) * timecode_scale_ns)
    tracks_element = segment.child(TRACKS_ID)
    if tracks_element is None:
        raise InputParseError("Tracks element is missing")
    tracks = _parse_tracks(tracks_element)
    known = {track.number: track for track in tracks}
    samples: dict[int, list[Sample]] = {track.number: [] for track in tracks if track.supported}
    clusters = segment.children_of(CLUSTER_ID)
    issues: list[dict] = []

    for cluster_index, cluster in enumerate(clusters):
        cluster_timecode = int(cluster.child_value(0xE7, 0))
        for child in cluster.children:
            block_group = child if child.element_id == BLOCK_GROUP_ID else None
            block = child.child(BLOCK_ID) if block_group else child
            if child.element_id not in (SIMPLE_BLOCK_ID, BLOCK_GROUP_ID) or block is None:
                continue
            parsed = _block_payload(block)
            if parsed is None:
                issues.append({
                    "code": "skipped_block",
                    "offset": block.offset,
                    "reason": "malformed block or unsupported lacing",
                })
                continue
            track_number, relative_ticks, flags, payload = parsed
            track = known.get(track_number)
            if track is None or not track.supported:
                issues.append({
                    "code": "unsupported_track_block",
                    "offset": block.offset,
                    "track_number": track_number,
                    "reason": "track missing or codec unsupported",
                })
                continue
            if block_group is not None:
                keyframe = block_group.child(0xFB) is None
                duration_ticks = _uint(block_group.child(0x9B))
                composition_offset = _block_addition_offset(block_group)
            else:
                keyframe = bool(flags & 0x80)
                duration_ticks = None
                composition_offset = 0
            samples[track_number].append(Sample(
                track_number=track_number,
                timestamp_ticks=cluster_timecode + relative_ticks,
                duration_ticks=duration_ticks,
                keyframe=keyframe,
                data=payload,
                cluster_index=cluster_index,
                offset=block.offset,
                composition_offset_ticks=composition_offset,
            ))

    cues_element = segment.child(CUES_ID)
    cues = _parse_cues(cues_element, segment, tracks) if cues_element else []
    return DemuxResult(
        parse=parse_result,
        segment=segment,
        tracks=tracks,
        samples=samples,
        timecode_scale_ns=timecode_scale_ns,
        duration_ns=duration_ns,
        cues=cues,
        cues_element_present=cues_element is not None,
        issues=issues,
        clusters=clusters,
    )


def demux_bytes(data: bytes) -> DemuxResult:
    return _parse_document(parse_ebml(data))


def demux_parsed(parse_result: ParseResult) -> DemuxResult:
    """Reuse an already parsed element tree for semantic extraction."""
    return _parse_document(parse_result)


def demux_file(path: str) -> DemuxResult:
    with open(path, "rb") as handle:
        return demux_bytes(handle.read())
