"""Matroska data records: tracks, blocks, clusters, cues, attachments."""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

from . import ebml_schema as ids
from .ebml_schema import SchemaError

DEFAULT_TIMESTAMP_SCALE = 1_000_000  # Matroska default: 1 ms ticks.
NANOS_PER_SECOND = 1_000_000_000


class ModelError(SchemaError):
    """Raised when a parsed tree cannot be assembled into a usable model."""

# ── tracks ────────────────────────────────────────────────────────────────


@dataclass
class VideoTrack:
    pixel_width: int
    pixel_height: int
    display_width: int | None = None
    display_height: int | None = None


@dataclass
class AudioTrack:
    sampling_frequency: float = 8000.0
    output_sampling_frequency: float | None = None
    channels: int = 1
    bit_depth: int | None = None


@dataclass
class Track:
    number: int
    uid: int | None
    uid_source: str  # "tag" | "hash" | "number"
    track_type: int
    codec_id: str
    language: str = "und"
    language_bcp47: str | None = None
    name: str | None = None
    default: bool = True
    forced: bool = False
    enabled: bool = True
    lacing: bool = False
    default_duration_ns: int | None = None
    codec_delay_ns: int = 0
    timestamp_scale: float = 1.0
    codec_private: bytes | None = None
    video: VideoTrack | None = None
    audio: AudioTrack | None = None
    compressed: bool = False
    source_offset: int = 0

    @property
    def type_name(self) -> str:
        return ids.TRACK_TYPE_NAMES.get(self.track_type, "unknown")

    @property
    def is_video(self) -> bool:
        return self.track_type == ids.TRACK_TYPE_VIDEO

    @property
    def is_audio(self) -> bool:
        return self.track_type == ids.TRACK_TYPE_AUDIO

    @property
    def is_subtitle(self) -> bool:
        return self.track_type == ids.TRACK_TYPE_SUBTITLE

    def as_dict(self) -> dict[str, object]:
        row: dict[str, object] = {
            "number": self.number,
            "uid": self.uid,
            "uid_source": self.uid_source,
            "type": self.type_name,
            "codec_id": self.codec_id,
            "language": self.language,
            "default": self.default,
            "forced": self.forced,
            "enabled": self.enabled,
            "source_offset": self.source_offset,
        }
        if self.name:
            row["name"] = self.name
        if self.default_duration_ns:
            row["default_duration_ns"] = self.default_duration_ns
        if self.codec_delay_ns:
            row["codec_delay_ns"] = self.codec_delay_ns
        if self.video:
            row["video"] = {
                "pixel_width": self.video.pixel_width,
                "pixel_height": self.video.pixel_height,
                "display_width": self.video.display_width,
                "display_height": self.video.display_height,
            }
        if self.audio:
            row["audio"] = {
                "sampling_frequency": self.audio.sampling_frequency,
                "channels": self.audio.channels,
                "bit_depth": self.audio.bit_depth,
            }
        if self.codec_private is not None:
            row["codec_private_bytes"] = len(self.codec_private)
        return row


# ── blocks ────────────────────────────────────────────────────────────────


@dataclass
class Frame:
    """One laced frame inside a Block/SimpleBlock."""

    index: int
    offset: int  # absolute source offset of the frame payload
    size: int


@dataclass
class Block:
    track_number: int
    relative_timecode: int
    cluster_timecode: int
    flags: int
    data_offset: int
    size: int
    frames: list[Frame]
    keyframe: bool
    invisible: bool
    discardable: bool
    lacing: str
    duration_ticks: int | None = None
    references: list[int] = field(default_factory=list)
    group_offset: int | None = None
    cluster_index: int = -1

    @property
    def timecode(self) -> int:
        """Absolute Cluster-relative Matroska timestamp in ticks."""
        return self.cluster_timecode + self.relative_timecode

    @property
    def frame_count(self) -> int:
        return len(self.frames)

    @property
    def payload_size(self) -> int:
        return sum(f.size for f in self.frames)

    def as_dict(self) -> dict[str, object]:
        return {
            "track_number": self.track_number,
            "timecode": self.timecode,
            "relative_timecode": self.relative_timecode,
            "keyframe": self.keyframe,
            "invisible": self.invisible,
            "discardable": self.discardable,
            "lacing": self.lacing,
            "frames": len(self.frames),
            "size": self.payload_size,
            "data_offset": self.data_offset,
            "duration_ticks": self.duration_ticks,
            "references": list(self.references),
        }


@dataclass
class Cluster:
    offset: int
    data_offset: int
    size: int
    unknown_size: bool
    timecode: int
    position: int | None
    blocks: list[Block] = field(default_factory=list)
    index: int = -1

    def blocks_for_track(self, track_number: int) -> list[Block]:
        return [b for b in self.blocks if b.track_number == track_number]


@dataclass
class CuePoint:
    time_ticks: int
    cluster_position: int
    track_number: int | None = None
    relative_position: int | None = None
    duration_ticks: int | None = None
    offset: int = 0

    def as_dict(self) -> dict[str, object]:
        return {
            "time_ticks": self.time_ticks,
            "cluster_position": self.cluster_position,
            "track_number": self.track_number,
            "relative_position": self.relative_position,
            "duration_ticks": self.duration_ticks,
        }


@dataclass
class Attachment:
    uid: int | None
    name: str | None
    mime_type: str | None
    description: str | None
    data_size: int
    sha256: str | None = None


@dataclass
class SegmentInfo:
    timestamp_scale_ns: int = DEFAULT_TIMESTAMP_SCALE
    duration_ticks: float | None = None
    title: str | None = None
    muxing_app: str | None = None
    writing_app: str | None = None
    segment_uid: bytes | None = None

    @property
    def duration_ns(self) -> float | None:
        if self.duration_ticks is None:
            return None
        return self.duration_ticks * self.timestamp_scale_ns


@dataclass
class MatroskaDocument:
    path: Path | None
    file_size: int
    segment_offset: int
    segment_data_offset: int
    segment_size: int | None
    segment_unknown_size: bool
    info: SegmentInfo
    tracks: list[Track]
    clusters: list[Cluster]
    cues: list[CuePoint]
    attachments: list[Attachment]
    element_count: int
    unknown_length_count: int
    warnings: list[dict[str, object]] = field(default_factory=list)

    # ── derived views ──────────────────────────────────────────────────
    def track_by_number(self, number: int) -> Track | None:
        for track in self.tracks:
            if track.number == number:
                return track
        return None

    def blocks_for_track(self, track: Track) -> list[Block]:
        out: list[Block] = []
        for cluster in self.clusters:
            out.extend(cluster.blocks_for_track(track.number))
        return out

    @property
    def all_blocks(self) -> list[Block]:
        return [b for cluster in self.clusters for b in cluster.blocks]

    @property
    def sample_count(self) -> int:
        return sum(len(block.frames) for block in self.all_blocks)

    @property
    def duration_ns(self) -> float | None:
        return self.info.duration_ns

    def summary(self) -> dict[str, object]:
        return {
            "file_size": self.file_size,
            "segment_offset": self.segment_offset,
            "segment_data_offset": self.segment_data_offset,
            "segment_size": self.segment_size,
            "segment_unknown_size": self.segment_unknown_size,
            "element_count": self.element_count,
            "unknown_length_count": self.unknown_length_count,
            "timecode_scale_ns": self.info.timestamp_scale_ns,
            "duration_ticks": self.info.duration_ticks,
            "track_count": len(self.tracks),
            "cluster_count": len(self.clusters),
            "cue_count": len(self.cues),
            "attachment_count": len(self.attachments),
            "block_count": len(self.all_blocks),
            "sample_count": self.sample_count,
            "warnings": len(self.warnings),
        }


