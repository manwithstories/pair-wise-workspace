"""Track mapping records: codec tables, errors, and mapped-track views."""

from __future__ import annotations

from dataclasses import dataclass, field


HANDLER_NAMES = {
    "video": "VideoHandler",
    "audio": "SoundHandler",
    "subtitle": "SubtitleHandler",
}

#: CodecID prefix → (mp4 sample entry fourcc, whether we can remux the samples).
#: Only passthrough-capable codecs are marked ``True``: we never re-encode.
CODEC_MAP: dict[str, tuple[bytes, bool]] = {
    "V_MPEG4/ISO/AVC": (b"avc1", True),
    "V_MPEGH/ISO/HEVC": (b"hvc1", True),
    "A_AAC": (b"mp4a", True),
    "A_AAC/MPEG4/LC": (b"mp4a", True),
    "A_OPUS": (b"Opus", True),
    "V_VP9": (b"vp09", False),
    "V_AV1": (b"av01", False),
    "S_TEXT/UTF8": (b"tx3g", False),
    "S_TEXT/ASS": (b"tx3g", False),
    "S_TEXT/SSA": (b"tx3g", False),
    "S_HDMV/PGS": (b"tx3g", False),
}


class TrackMappingError(ValueError):
    """Raised when a document cannot be mapped without losing required tracks."""

    def __init__(self, code: str, message: str, track_number: int | None = None) -> None:
        self.code = code
        self.track_number = track_number
        super().__init__(message)

    def as_dict(self) -> dict[str, object]:
        return {"code": self.code, "message": str(self), "track_number": self.track_number}


@dataclass
class MappedTrack:
    """One Matroska track and the MP4 track it becomes."""

    source_number: int
    uid: int
    uid_source: str
    track_type: str
    codec_id: str
    language: str
    default: bool
    forced: bool
    source_index: int
    mp4_track_id: int
    handler_name: str
    sample_entry: str
    remuxable: bool
    width: int | None = None
    height: int | None = None
    channels: int | None = None
    sample_rate: float | None = None
    codec_private: bytes | None = None
    sample_count: int = 0
    notes: list[str] = field(default_factory=list)

    def as_dict(self) -> dict[str, object]:
        row: dict[str, object] = {
            "source_track_number": self.source_number,
            "mp4_track_id": self.mp4_track_id,
            "track_uid": self.uid,
            "uid_source": self.uid_source,
            "type": self.track_type,
            "codec_id": self.codec_id,
            "sample_entry": self.sample_entry,
            "language": self.language,
            "default": self.default,
            "forced": self.forced,
            "remuxable": self.remuxable,
            "sample_count": self.sample_count,
            "handler_name": self.handler_name,
            "source_index": self.source_index,
        }
        if self.width and self.height:
            row["dimensions"] = {"width": self.width, "height": self.height}
        if self.channels:
            row["audio"] = {"channels": self.channels, "sample_rate": self.sample_rate}
        if self.notes:
            row["notes"] = list(self.notes)
        return row


@dataclass
class TrackMapping:
    """The full mapping table plus the evidence behind every decision."""

    tracks: list[MappedTrack]
    skipped: list[dict[str, object]]
    warnings: list[dict[str, object]]

    def by_source_number(self, number: int) -> MappedTrack:
        for item in self.tracks:
            if item.source_number == number:
                return item
        raise TrackMappingError("track_not_mapped", f"track {number} is not in the mapping", number)

    def by_uid(self, uid: int) -> MappedTrack | None:
        for item in self.tracks:
            if item.uid == uid:
                return item
        return None

    @property
    def remuxable(self) -> list[MappedTrack]:
        return [t for t in self.tracks if t.remuxable]

    def summary(self) -> dict[str, object]:
        return {
            "track_count": len(self.tracks),
            "remuxable_count": len(self.remuxable),
            "skipped_count": len(self.skipped),
            "tracks": [t.as_dict() for t in self.tracks],
            "skipped": list(self.skipped),
            "warnings": list(self.warnings),
        }


__all__ = [
    "CODEC_MAP",
    "HANDLER_NAMES",
    "MappedTrack",
    "TrackMapping",
    "TrackMappingError",
]
