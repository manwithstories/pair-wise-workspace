"""Public MP4 box surface: build a movie, serialize it, read it back.

This facade is what the remuxer and verifier import. The implementation is
split into :mod:`src._mp4_core` (tree + parser), :mod:`src._mp4_samples`
(sample entries), and :mod:`src._mp4_movie` (moov/trak/stbl assembly).
"""

from __future__ import annotations

from ._mp4_core import (
    CONTAINER_TYPES,
    FULL_BOX_TYPES,
    SAMPLE_ENTRY_TYPES,
    Box,
    BoxRecord,
    Mp4Error,
    assign_offsets,
    box,
    full_box,
    parse_boxes,
    serialize,
)
from ._mp4_movie import (
    MAX_32BIT_OFFSET,
    MOVIE_TIMESCALE,
    MovieLayout,
    MovieSpec,
    build_layout,
    build_moov,
    build_stbl,
    ftyp_box,
    mdat_header,
    sample_entry_for,
)
from ._mp4_samples import (
    AudioEntry,
    VisualEntry,
    avc1_entry,
    esds_box,
    hvc1_entry,
    mp4a_entry,
    opus_entry,
    stsd_box,
    tx3g_entry,
)

__all__ = [
    "AudioEntry",
    "Box",
    "BoxRecord",
    "CONTAINER_TYPES",
    "FULL_BOX_TYPES",
    "MAX_32BIT_OFFSET",
    "MOVIE_TIMESCALE",
    "Mp4Error",
    "MovieLayout",
    "MovieSpec",
    "SAMPLE_ENTRY_TYPES",
    "VisualEntry",
    "assign_offsets",
    "avc1_entry",
    "box",
    "build_layout",
    "build_moov",
    "build_stbl",
    "esds_box",
    "ftyp_box",
    "full_box",
    "hvc1_entry",
    "mdat_header",
    "mp4a_entry",
    "opus_entry",
    "parse_boxes",
    "sample_entry_for",
    "serialize",
    "stsd_box",
    "tx3g_entry",
]
