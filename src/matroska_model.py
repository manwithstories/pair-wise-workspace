"""Matroska document model: Segment, Info, Tracks, Clusters, Cues, Attachments.

The model is built from an :class:`~src.ebml_reader.Element` tree and keeps the
absolute file offsets of every block and cue. That offset fidelity is what the
remuxer and verifier audit against later: a track mapping is only trustworthy
when every sample can be traced back to its source byte range.

Implementation is split by concern:
  * :mod:`src._matroska_types`   — dataclasses and shared constants
  * :mod:`src._matroska_block`   — Block payload decoding and Cluster assembly
  * :mod:`src._matroska_track`   — TrackEntry decoding
  * :mod:`src._matroska_build`   — document assembly (Info, Cues, Attachments)
"""

from __future__ import annotations

from ._matroska_block import build_cluster, parse_block_payload
from ._matroska_build import build_attachments, build_cues, build_document, build_info
from ._matroska_track import build_track
from ._matroska_types import (
    DEFAULT_TIMESTAMP_SCALE,
    NANOS_PER_SECOND,
    Attachment,
    AudioTrack,
    Block,
    Cluster,
    CuePoint,
    Frame,
    MatroskaDocument,
    ModelError,
    SegmentInfo,
    Track,
    VideoTrack,
)

__all__ = [
    "Attachment",
    "AudioTrack",
    "Block",
    "Cluster",
    "CuePoint",
    "DEFAULT_TIMESTAMP_SCALE",
    "Frame",
    "MatroskaDocument",
    "ModelError",
    "NANOS_PER_SECOND",
    "SegmentInfo",
    "Track",
    "VideoTrack",
    "build_attachments",
    "build_cluster",
    "build_cues",
    "build_document",
    "build_info",
    "build_track",
    "parse_block_payload",
]
