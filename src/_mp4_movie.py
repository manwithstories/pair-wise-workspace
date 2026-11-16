"""Movie box construction: ftyp, moov, per-track trak trees, and mdat.

The builder is pure — it takes the index tables produced by
:mod:`src.index_rebuilder` and returns bytes plus the box records it expects to
be able to read back. Nothing here opens files; writing and verification live in
the remux/verify layers.

Layout chosen deliberately for verifiability:
  ``ftyp | moov | mdat``
Chunk offsets are absolute into the finished file, so the builder needs the
exact moov size before it can place mdat. It therefore builds the moov twice:
once to measure, once with the offsets filled in (offsets never change the moov
size because 32-bit stco entries are fixed-width).
"""

from __future__ import annotations

import struct
from dataclasses import dataclass, field

from ._mp4_core import Box, Mp4Error, assign_offsets, box, full_box, serialize
from ._mp4_tables import build_stbl, build_stbl_placeholder, sample_entry_for
from .index_rebuilder import TrackIndex
from .track_mapper import MappedTrack

MOVIE_TIMESCALE = 1_000
MAX_32BIT_OFFSET = 0xFFFFFFFF

BRAND_MAJOR = b"isom"
BRAND_MINOR = 512
BRAND_COMPATIBLE = b"isomiso2avc1mp41"


@dataclass
class MovieLayout:
    """Byte layout of a finished MP4 file."""

    ftyp: Box
    moov: Box
    mdat_offset: int
    mdat_size: int
    box_count: int = 0
    records: list = field(default_factory=list)

    @property
    def total_size(self) -> int:
        return self.mdat_offset + 8 + self.mdat_size

    @property
    def media_data_offset(self) -> int:
        return self.mdat_offset + 8


def ftyp_box() -> Box:
    payload = BRAND_MAJOR + BRAND_MINOR.to_bytes(4, "big") + BRAND_COMPATIBLE
    return box(b"ftyp", payload)


def _mvhd(timescale: int, duration: int, next_track_id: int) -> Box:
    payload = struct.pack(">IIII", 0, 0, timescale, duration)
    payload += struct.pack(">i", 0x00010000)  # rate 1.0
    payload += struct.pack(">h", 0x0100)  # volume 1.0
    payload += b"\x00" * 2 + b"\x00" * 8
    payload += _identity_matrix()
    payload += b"\x00" * 24  # pre_defined[6]
    payload += next_track_id.to_bytes(4, "big")
    return full_box(b"mvhd", 0, 0, payload)


def _identity_matrix() -> bytes:
    return struct.pack(
        ">9i",
        0x00010000, 0, 0,
        0, 0x00010000, 0,
        0, 0, 0x40000000,
    )


def _tkhd(track_id: int, duration: int, width: int = 0, height: int = 0, *, enabled: bool = True, flags_extra: int = 0) -> Box:
    flags = (0x000001 if enabled else 0) | flags_extra
    payload = struct.pack(">IIIII", 0, 0, track_id, 0, duration)
    payload += b"\x00" * 8  # reserved
    payload += struct.pack(">hhhh", 0, 0, 0x0100 if width else 0, 0)  # layer, group, volume, reserved
    payload += _identity_matrix()
    payload += struct.pack(">II", width << 16, height << 16)
    return full_box(b"tkhd", 0, flags, payload)


def _mdhd(timescale: int, duration: int, language: str) -> Box:
    payload = struct.pack(">IIII", 0, 0, timescale, duration)
    payload += struct.pack(">HH", _pack_language(language), 0)
    return full_box(b"mdhd", 0, 0, payload)


def _pack_language(language: str) -> int:
    """ISO-639-2/T packed as 3×5 bits; 'und' is the legal fallback."""
    code = language.lower()
    if len(code) != 3 or not code.isalpha():
        code = "und"
    value = 0
    for char in code:
        value = (value << 5) | (ord(char) - 0x60)
    return value


def _hdlr(handler_type: bytes, name: str) -> Box:
    payload = b"\x00" * 4 + handler_type + b"\x00" * 12 + name.encode("utf-8") + b"\x00"
    return full_box(b"hdlr", 0, 0, payload)


def _vmhd() -> Box:
    return full_box(b"vmhd", 0, 1, b"\x00" * 8)


def _smhd(balance: int = 0) -> Box:
    return full_box(b"smhd", 0, 0, struct.pack(">hH", balance, 0))


def _nmhd() -> Box:
    return full_box(b"nmhd", 0, 0, b"")


def _dinf() -> Box:
    url = full_box(b"url ", 0, 1, b"")
    dref = full_box(b"dref", 0, 0, (1).to_bytes(4, "big"), children=[url])
    return box(b"dinf", children=[dref])


@dataclass
class MovieSpec:
    """Everything the moov needs, independent of file offsets."""

    indexes: list[TrackIndex]
    mapped: list[MappedTrack]
    movie_timescale: int = MOVIE_TIMESCALE
    duration_override: int | None = None

    def by_number(self, number: int) -> tuple[TrackIndex, MappedTrack]:
        for index, mapped in zip(self.indexes, self.mapped):
            if index.track_number == number:
                return index, mapped
        raise Mp4Error("track_missing", f"track {number} is not in the movie spec")


def _build_trak(index: TrackIndex, mapped: MappedTrack, movie_duration: int, *, chunk_offsets=None) -> Box:
    width = (mapped.width or 0) if mapped.track_type == "video" else 0
    height = (mapped.height or 0) if mapped.track_type == "video" else 0
    tkhd = _tkhd(mapped.mp4_track_id, movie_duration, width, height, enabled=mapped.default)
    mdhd = _mdhd(index.timescale, index.media_duration, mapped.language)
    hdlr = _hdlr(
        {"video": b"vide", "audio": b"soun"}.get(mapped.track_type, b"text"),
        f"{mapped.handler_name} ({mapped.codec_id})",
    )
    if mapped.track_type == "video":
        header = _vmhd()
    elif mapped.track_type == "audio":
        header = _smhd()
    else:
        header = _nmhd()
    dinf = _dinf()
    stbl = build_stbl(index, mapped, chunk_offsets=chunk_offsets)
    minf = box(b"minf", children=[header, dinf, stbl])
    mdia = box(b"mdia", children=[mdhd, hdlr, minf])
    return box(b"trak", children=[tkhd, mdia])


def build_moov(
    spec: MovieSpec, *, chunk_offsets: dict[int, list[int]] | None = None
) -> Box:
    """Build the moov box, optionally with real chunk offsets per track."""
    durations = [index.media_duration for index in spec.indexes]
    movie_duration = spec.duration_override or _max_duration(durations, spec.movie_timescale, spec.indexes)
    next_track_id = max((m.mp4_track_id for m in spec.mapped), default=0) + 1
    children: list[Box] = [_mvhd(spec.movie_timescale, movie_duration, next_track_id)]
    for index, mapped in zip(spec.indexes, spec.mapped):
        offsets = None
        if chunk_offsets is not None:
            offsets = chunk_offsets.get(index.track_number)
        children.append(_build_trak(index, mapped, movie_duration, chunk_offsets=offsets))
    return box(b"moov", children=children)


def _max_duration(durations: list[int], movie_timescale: int, indexes: list[TrackIndex]) -> int:
    best = 0
    for duration, index in zip(durations, indexes):
        if index.timescale <= 0:
            continue
        scaled = int(round(duration * movie_timescale / index.timescale))
        best = max(best, scaled)
    return best


def build_layout(spec: MovieSpec) -> MovieLayout:
    """Measure the moov, then rebuild it with absolute chunk offsets."""
    placeholder = build_moov(spec)
    ftyp = ftyp_box()
    mdat_offset = len(serialize(ftyp)) + len(serialize(placeholder))
    mdat_header = 8
    mdat_data_offset = mdat_offset + mdat_header

    # Assign output offsets by walking the interleaved chunk plan.
    from .index_rebuilder import lay_out_media, total_media_bytes

    lay_out_media(spec.indexes, mdat_data_offset=mdat_data_offset)
    chunk_offsets = {
        index.track_number: [chunk.offset for chunk in index.chunks] for index in spec.indexes
    }
    moov = build_moov(spec, chunk_offsets=chunk_offsets)
    moov_bytes = serialize(moov)
    if len(moov_bytes) != len(serialize(placeholder)):
        # Offsets are fixed-width, so a size change means a real bug (e.g. an
        # offset crossed into co64). Recompute once with the true layout.
        mdat_offset = len(serialize(ftyp)) + len(moov_bytes)
        lay_out_media(spec.indexes, mdat_data_offset=mdat_offset + mdat_header)
        chunk_offsets = {
            index.track_number: [chunk.offset for chunk in index.chunks] for index in spec.indexes
        }
        moov = build_moov(spec, chunk_offsets=chunk_offsets)

    media_size = total_media_bytes(spec.indexes)
    records = assign_offsets(ftyp, 0) + assign_offsets(moov, len(serialize(ftyp)))
    # mdat is one more box in the finished file; the record tree only covers
    # the header boxes because mdat's payload is streamed, not serialized.
    count = sum(len(list(record.walk())) for record in records) + 1
    return MovieLayout(
        ftyp=ftyp,
        moov=moov,
        mdat_offset=mdat_offset,
        mdat_size=media_size,
        box_count=count,
        records=records,
    )


def mdat_header(size: int) -> bytes:
    """mdat box header for ``size`` payload bytes (largesize when needed)."""
    total = size + 8
    if total > MAX_32BIT_OFFSET:
        return struct.pack(">I4sQ", 1, b"mdat", size + 16)
    return struct.pack(">I4s", total, b"mdat")

__all__ = [
    "MAX_32BIT_OFFSET",
    "MOVIE_TIMESCALE",
    "MovieLayout",
    "MovieSpec",
    "build_layout",
    "build_moov",
    "build_stbl",
    "build_stbl_placeholder",
    "ftyp_box",
    "mdat_header",
    "sample_entry_for",
]
