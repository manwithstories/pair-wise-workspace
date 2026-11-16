"""TrackEntry decoding into :class:`Track` records."""

from __future__ import annotations

from . import ebml_schema as ids
from ._matroska_types import AudioTrack, ModelError, Track, VideoTrack
from .ebml_schema import decode_payload, decode_uint
from .ebml_reader import Element


def _uint_child(parent: Element, eid: bytes, default: int | None = None) -> int | None:
    child = parent.child(eid)
    if child is None or child.data is None:
        return default
    return decode_uint(child.data)


def decode_utf8_safe(data: bytes) -> str:
    return data.split(b"\x00", 1)[0].decode("utf-8", errors="replace")


def decode_ascii_safe(data: bytes) -> str:
    return data.split(b"\x00", 1)[0].decode("ascii", errors="replace")


def _str_child(parent: Element, eid: bytes, decoder=decode_utf8_safe) -> str | None:
    child = parent.child(eid)
    if child is None or child.data is None:
        return None
    return decoder(child.data)


def _float_child(parent: Element, eid: bytes, default: float | None = None) -> float | None:
    child = parent.child(eid)
    if child is None or child.data is None:
        return default
    return decode_payload("float", child.data)


def build_track(entry: Element, *, warnings: list[dict[str, object]]) -> Track:
    number = _uint_child(entry, ids.TRACK_NUMBER)
    if number is None:
        raise ModelError("track_number_missing", "TrackEntry without TrackNumber", entry.offset)
    uid = _uint_child(entry, ids.TRACK_UID)
    uid_source = "tag"
    if uid is None:
        uid_source = "number"
        warnings.append(
            {
                "code": "track_uid_missing",
                "message": f"track {number} has no TrackUID; falling back to TrackNumber",
                "offset": entry.offset,
            }
        )
    track_type = _uint_child(entry, ids.TRACK_TYPE)
    if track_type is None:
        raise ModelError("track_type_missing", f"track {number} has no TrackType", entry.offset)
    codec_id = _str_child(entry, ids.CODEC_ID, decode_ascii_safe) or ""
    if not codec_id:
        raise ModelError("track_codec_missing", f"track {number} has no CodecID", entry.offset)

    language = _str_child(entry, ids.LANGUAGE, decode_ascii_safe) or "und"
    bcp47 = _str_child(entry, ids.LANGUAGE_BCP47, decode_ascii_safe)
    default = bool(_uint_child(entry, ids.FLAG_DEFAULT, 1))
    forced = bool(_uint_child(entry, ids.FLAG_FORCED, 0))
    enabled = bool(_uint_child(entry, ids.FLAG_ENABLED, 1))
    lacing = bool(_uint_child(entry, ids.FLAG_LACING, 0))
    default_duration = _uint_child(entry, ids.DEFAULT_DURATION)
    codec_delay = _uint_child(entry, ids.CODE_C_DELAY, 0) or 0
    ts_scale = _float_child(entry, ids.TRACK_TIMESTAMP_SCALE, 1.0) or 1.0
    codec_private_el = entry.child(ids.CODEC_PRIVATE)
    codec_private = codec_private_el.data if codec_private_el is not None else None

    video = None
    video_el = entry.child(ids.VIDEO)
    if video_el is not None:
        width = _uint_child(video_el, ids.PIXEL_WIDTH)
        height = _uint_child(video_el, ids.PIXEL_HEIGHT)
        if width is None or height is None:
            raise ModelError(
                "video_dimensions_missing",
                f"track {number} Video element lacks pixel dimensions",
                video_el.offset,
            )
        video = VideoTrack(
            pixel_width=width,
            pixel_height=height,
            display_width=_uint_child(video_el, ids.DISPLAY_WIDTH),
            display_height=_uint_child(video_el, ids.DISPLAY_HEIGHT),
        )

    audio = None
    audio_el = entry.child(ids.AUDIO)
    if audio_el is not None:
        audio = AudioTrack(
            sampling_frequency=_float_child(audio_el, ids.SAMPLING_FREQUENCY, 8000.0) or 8000.0,
            output_sampling_frequency=_float_child(audio_el, ids.OUTPUT_SAMPLING_FREQUENCY),
            channels=_uint_child(audio_el, ids.CHANNELS, 1) or 1,
            bit_depth=_uint_child(audio_el, ids.BIT_DEPTH),
        )

    compressed = False
    encodings = entry.child(ids.CONTENT_ENCODINGS)
    if encodings is not None:
        for encoding in encodings.children_named(ids.CONTENT_ENCODING):
            comp = encoding.child(ids.CONTENT_COMPRESSION)
            if comp is None:
                continue
            algo = _uint_child(comp, ids.CONTENT_COMP_ALGO, 0)
            if algo != 0:
                compressed = True
                warnings.append(
                    {
                        "code": "track_compressed",
                        "message": f"track {number} uses compression algorithm {algo}",
                        "offset": encoding.offset,
                    }
                )

    return Track(
        number=number,
        uid=uid,
        uid_source=uid_source,
        track_type=track_type,
        codec_id=codec_id,
        language=language,
        language_bcp47=bcp47,
        name=_str_child(entry, ids.NAME),
        default=default,
        forced=forced,
        enabled=enabled,
        lacing=lacing,
        default_duration_ns=default_duration,
        codec_delay_ns=codec_delay,
        timestamp_scale=ts_scale,
        codec_private=codec_private,
        video=video,
        audio=audio,
        compressed=compressed,
        source_offset=entry.offset,
    )


