"""MP4 box surface: definitions, reading, and offset assignment."""

from __future__ import annotations

from ._mp4_defs import (
    CONTAINER_TYPES,
    FULL_BOX_TYPES,
    SAMPLE_ENTRY_FIXED,
    SAMPLE_ENTRY_TYPES,
    Box,
    Mp4Error,
    box,
    full_box,
    serialize,
)
from ._mp4_read import BoxRecord, assign_offsets, parse_boxes


__all__ = [
    "Box",
    "BoxRecord",
    "CONTAINER_TYPES",
    "FULL_BOX_TYPES",
    "Mp4Error",
    "SAMPLE_ENTRY_FIXED",
    "SAMPLE_ENTRY_TYPES",
    "assign_offsets",
    "box",
    "full_box",
    "parse_boxes",
    "serialize",
]
