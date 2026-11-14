"""Semantic model for the Matroska subset used by this project."""

from __future__ import annotations

from dataclasses import dataclass, field


def ticks_to_ns(ticks: int, timestamp_scale_ns: int = 1_000_000) -> int:
    return ticks * timestamp_scale_ns


@dataclass(slots=True)
class Info:
    timestamp_scale_ns: int = 1_000_000
    duration_ticks: float | None = None
    muxing_app: str = ""
    writing_app: str = ""
    title: str = ""


@dataclass(slots=True)
class Track:
    number: int
    uid: int = 0
    track_type: int = 0
    codec_id: str = ""
    codec_private: bytes = b""
    language: str = "und"
    name: str = ""
    default: bool = True
    forced: bool = False
    default_duration_ns: int | None = None
    pixel_width: int = 0
    pixel_height: int = 0
    sampling_frequency: float = 0.0
    channels: int = 0

    @property
    def handler(self) -> str:
        if self.track_type == 1:
            return "vide"
        if self.track_type == 2:
            return "soun"
        if self.track_type == 17:
            return "sbtl"
        return "meta"


@dataclass(slots=True)
class Tracks:
    entries: list[Track] = field(default_factory=list)


@dataclass(slots=True)
class Block:
    track_number: int
    relative_ticks: int
    payload: bytes
    keyframe: bool = True
    flags: int = 0
    duration_ticks: int | None = None
    reference_blocks: tuple[int, ...] = ()
    source_offset: int = 0
    cluster_index: int = 0


@dataclass(slots=True)
class BlockGroup:
    block: Block
    duration_ticks: int | None = None
    reference_blocks: tuple[int, ...] = ()


@dataclass(slots=True)
class Cluster:
    timestamp_ticks: int = 0
    offset: int = 0
    blocks: list[Block | BlockGroup] = field(default_factory=list)


@dataclass(slots=True)
class Cue:
    time_ticks: int
    track_number: int
    cluster_offset: int


@dataclass(slots=True)
class Cues:
    points: list[Cue] = field(default_factory=list)


@dataclass(slots=True)
class Chapter:
    uid: int
    start_ns: int
    end_ns: int | None = None
    title: str = ""
    language: str = "und"


@dataclass(slots=True)
class Chapters:
    atoms: list[Chapter] = field(default_factory=list)


@dataclass(slots=True)
class MatroskaDocument:
    info: Info = field(default_factory=Info)
    tracks: list[Track] = field(default_factory=list)
    clusters: list[Cluster] = field(default_factory=list)
    cues: list[Cue] = field(default_factory=list)
    chapters: list[Chapter] = field(default_factory=list)

