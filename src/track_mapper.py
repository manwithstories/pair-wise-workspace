"""Stable track mapping from Matroska track identity to MP4 track identity.

Two ideas drive this module:

1. **TrackUID is the identity.** TrackNumber is a display/ordering hint that
   remuxers are free to renumber; TrackUID is the durable key. The mapping is
   keyed on TrackUID whenever the source provides one.
2. **Downgrades are reported, never silent.** When a TrackUID is absent we
   derive a deterministic hash from stable attributes, mark
   ``uid_source="hash"``, and record the reason. A consumer can then decide
   whether the mapping is strong enough to trust.

Subtitles are mapped but *not* carried into MP4 as timed text samples; the map
marks their disposition so the remux report can explain the omission instead of
silently losing tracks.
"""

from __future__ import annotations

import hashlib

from ._track_records import (
    CODEC_MAP,
    HANDLER_NAMES,
    MappedTrack,
    TrackMapping,
    TrackMappingError,
)
from .matroska_model import MatroskaDocument, Track


def derive_uid(track: Track, document_uid: bytes | None) -> tuple[int, str]:
    """Deterministic fallback UID when the source omits TrackUID.

    Hashes the stable identity attributes (type, codec, language, dimensions,
    source index) so the same track maps to the same value across runs, and
    marks the value as derived so the report can call out the downgrade.
    """
    parts = [
        str(track.track_type),
        track.codec_id,
        track.language,
        str(track.number),
    ]
    if track.video:
        parts += [str(track.video.pixel_width), str(track.video.pixel_height)]
    if track.audio:
        parts += [str(track.audio.channels), f"{track.audio.sampling_frequency:.3f}"]
    if document_uid:
        parts.append(document_uid.hex())
    digest = hashlib.sha256("|".join(parts).encode("utf-8")).digest()
    value = int.from_bytes(digest[:8], "big") & ((1 << 63) - 1)
    if value == 0:
        value = 1
    return value, "hash"


def _codec_entry(track: Track) -> tuple[bytes, bool]:
    if track.codec_id in CODEC_MAP:
        return CODEC_MAP[track.codec_id]
    prefix = track.codec_id.split("/")[0]
    if prefix in CODEC_MAP:
        return CODEC_MAP[prefix]
    return b"", False

def build_track_mapping(
    document: MatroskaDocument,
    *,
    include_subtitles: bool = True,
    strict_codecs: bool = False,
) -> TrackMapping:
    """Map every source track to a stable MP4 track identity.

    ``strict_codecs`` promotes an unsupported CodecID from a warning to a hard
    failure, which is what the strict CLI mode uses: a partial remux is worse
    than no remux when the caller asked for fidelity.
    """
    warnings: list[dict[str, object]] = []
    skipped: list[dict[str, object]] = []
    mapped: list[MappedTrack] = []
    used_uids: dict[int, int] = {}
    used_numbers: set[int] = set()
    next_track_id = 1

    for index, track in enumerate(document.tracks):
        if track.number in used_numbers:
            raise TrackMappingError(
                "track_number_conflict", f"duplicate track number {track.number}", track.number
            )
        used_numbers.add(track.number)

        if track.uid is not None and track.uid_source == "tag":
            uid = track.uid
            uid_source = "tag"
        else:
            uid, uid_source = derive_uid(track, document.info.segment_uid)
            warnings.append(
                {
                    "code": "track_uid_derived",
                    "message": f"track {track.number} has no TrackUID; derived {uid:#x} from attributes",
                    "track_number": track.number,
                    "uid_source": uid_source,
                }
            )
        if uid in used_uids:
            raise TrackMappingError(
                "track_uid_conflict",
                f"track {track.number} resolves to TrackUID {uid:#x} already used by track "
                f"{used_uids[uid]}",
                track.number,
            )
        used_uids[uid] = track.number

        entry, passthrough = _codec_entry(track)
        remuxable = passthrough
        notes: list[str] = []

        if track.compressed:
            remuxable = False
            notes.append("track has ContentCompression; compressed samples cannot be passed through")
        if track.is_subtitle and include_subtitles:
            notes.append("subtitle track is mapped but not written as MP4 timed text")
            remuxable = False
        if not entry:
            if strict_codecs:
                raise TrackMappingError(
                    "unsupported_codec",
                    f"track {track.number} CodecID {track.codec_id!r} is not supported for remux",
                    track.number,
                )
            notes.append(f"CodecID {track.codec_id!r} has no known MP4 sample entry")
            warnings.append(
                {
                    "code": "unsupported_codec",
                    "message": f"track {track.number} CodecID {track.codec_id!r} is not remuxable",
                    "track_number": track.number,
                }
            )
            remuxable = False
        if not remuxable:
            skipped.append(
                {
                    "source_track_number": track.number,
                    "codec_id": track.codec_id,
                    "type": track.type_name,
                    "reason": notes[-1] if notes else "not remuxable",
                }
            )

        mapped.append(
            MappedTrack(
                source_number=track.number,
                uid=uid,
                uid_source=uid_source,
                track_type=track.type_name,
                codec_id=track.codec_id,
                language=(track.language_bcp47 or track.language or "und").lower(),
                default=track.default,
                forced=track.forced,
                source_index=index,
                mp4_track_id=next_track_id,
                handler_name=HANDLER_NAMES.get(track.type_name, "UnknownHandler"),
                sample_entry=entry.decode("ascii") if entry else "",
                remuxable=remuxable,
                width=track.video.pixel_width if track.video else None,
                height=track.video.pixel_height if track.video else None,
                channels=track.audio.channels if track.audio else None,
                sample_rate=track.audio.sampling_frequency if track.audio else None,
                codec_private=track.codec_private,
                sample_count=len(document.blocks_for_track(track)),
                notes=notes,
            )
        )
        next_track_id += 1

    if not any(t.remuxable for t in mapped):
        raise TrackMappingError(
            "no_remuxable_tracks", "no track can be carried into MP4 without re-encoding"
        )

    return TrackMapping(tracks=mapped, skipped=skipped, warnings=warnings)


def _language_code(language: str) -> str:
    """Best-effort BCP-47→ISO-639-2/T for MP4 (3 chars, lowercase)."""
    primary = language.split("-")[0].lower()
    if len(primary) == 3:
        return primary
    if len(primary) == 2:
        # mdhd wants ISO-639-2/T; without a table, pad is wrong, so fall back.
        return "und"
    return "und"


__all__ = [
    "CODEC_MAP",
    "HANDLER_NAMES",
    "MappedTrack",
    "TrackMapping",
    "TrackMappingError",
    "build_track_mapping",
    "derive_uid",
]
