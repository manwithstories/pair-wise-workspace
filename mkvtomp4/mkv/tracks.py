"""TrackEntry parsing and the shared media index used by both exporters."""

from __future__ import annotations

from dataclasses import dataclass, field

from mkvtomp4.mkv.tree import (
    ElementNode,
    MatroskaTree,
    float_value,
    payload_bytes,
    string_value,
    uint_value,
)

TRACK_TYPE_VIDEO = 1
TRACK_TYPE_AUDIO = 2

CODEC_MAP = {
    "V_MPEG4/ISO/AVC": "avc1",
    "V_MPEGH/ISO/HEVC": "hvc1",
    "V_AV1": "av01",
    "A_AAC": "mp4a",
    "A_OPUS": "Opus",
}


class NoUsableTracksError(ValueError):
    """Raised when no supported track can be produced."""


class ZeroSamplesError(ValueError):
    """Raised when a mapped track has no source samples."""


@dataclass
class TrackInfo:
    mkv_track: int
    mp4_track_id: int
    track_type: int
    codec: str
    mp4_codec: str
    codec_private: bytes
    default_duration_ns: int
    timescale: int = 1000
    width: int = 0
    height: int = 0
    sample_rate: float = 0.0
    channels: int = 0
    language: str = ""
    name: str = ""

    @property
    def frame_duration_units(self) -> int:
        if self.default_duration_ns <= 0:
            return 1
        value = self.default_duration_ns * self.timescale / 1_000_000_000
        return max(1, int(round(value)))


@dataclass
class SkippedTrack:
    mkv_track: int
    codec: str
    track_type: int
    reason: str


@dataclass
class BlockSample:
    track_number: int
    pts_ms: float
    dts_ms: float
    duration_ms: float
    keyframe: bool
    source_offset: int
    size: int
    sequence: int
    segment_index: int


@dataclass
class CueStats:
    entries: int = 0
    valid: int = 0
    rejected: int = 0
    index_source: str = "scan"
    rebuilt_samples: int = 0


@dataclass
class MediaIndex:
    path: str
    tree: MatroskaTree
    tracks: list[TrackInfo]
    skipped: list[SkippedTrack]
    samples: dict[int, list[BlockSample]]
    timecode_scale_ns: int
    cue_stats: CueStats
    warnings: list[dict[str, object]] = field(default_factory=list)
    segment: ElementNode | None = None

    @property
    def sample_count(self) -> int:
        return sum(len(items) for items in self.samples.values())


def _parse_track_entry(
    tree: MatroskaTree,
    entry: ElementNode,
    mp4_track_id: int,
) -> TrackInfo | SkippedTrack:
    number = uint_value(tree, entry.first(0xD7))
    track_type = uint_value(tree, entry.first(0x83))
    codec = string_value(tree, entry.first(0x86))
    if track_type not in (TRACK_TYPE_VIDEO, TRACK_TYPE_AUDIO):
        return SkippedTrack(number, codec, track_type, f"unsupported track type {track_type}")
    mp4_codec = CODEC_MAP.get(codec)
    if mp4_codec is None:
        return SkippedTrack(number, codec, track_type, "unsupported codec")
    private_node = entry.first(0x63A2)
    codec_private = b""
    if private_node is not None and private_node.size is not None:
        codec_private = payload_bytes(tree, private_node)
    video = entry.first(0xE0)
    audio = entry.first(0xE1)
    return TrackInfo(
        mkv_track=number,
        mp4_track_id=mp4_track_id,
        track_type=track_type,
        codec=codec,
        mp4_codec=mp4_codec,
        codec_private=codec_private,
        default_duration_ns=uint_value(tree, entry.first(0x23E383)),
        width=uint_value(tree, video.first(0xB0)) if video else 0,
        height=uint_value(tree, video.first(0xBA)) if video else 0,
        sample_rate=float_value(tree, audio.first(0xB5)) if audio else 0.0,
        channels=uint_value(tree, audio.first(0x9F)) if audio else 0,
        language=string_value(tree, entry.first(0x22B59C), "und"),
        name=string_value(tree, entry.first(0x536E)),
    )


def parse_tracks(
    tree: MatroskaTree,
) -> tuple[list[TrackInfo], list[SkippedTrack], ElementNode | None]:
    for segment in tree.segments():
        tracks_node = segment.first(0x1654AE6B)
        if tracks_node is None:
            continue
        mapped: list[TrackInfo] = []
        skipped: list[SkippedTrack] = []
        for entry in tracks_node.find_all(0xAE):
            result = _parse_track_entry(tree, entry, len(mapped) + 1)
            if isinstance(result, TrackInfo):
                if result.mkv_track <= 0:
                    skipped.append(
                        SkippedTrack(0, result.codec, result.track_type, "missing TrackNumber")
                    )
                else:
                    mapped.append(result)
            else:
                skipped.append(result)
        if not mapped:
            raise NoUsableTracksError("TrackEntry present but no supported audio/video track")
        return mapped, skipped, segment
    raise NoUsableTracksError("no Tracks/TrackEntry found")


def timecode_scale_ns(tree: MatroskaTree, segment: ElementNode) -> int:
    info = segment.first(0x1549A966)
    scale = uint_value(tree, info.first(0x2AD7B1)) if info else 0
    return scale or 1_000_000
