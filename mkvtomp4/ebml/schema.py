"""Matroska/EBML element identifiers and structural metadata."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class ElementSpec:
    eid: int
    name: str
    kind: str


_SPECS = (
    (0x1A45DFA3, "EBML", "master"),
    (0x4286, "EBMLVersion", "uint"),
    (0x42F7, "EBMLReadVersion", "uint"),
    (0x42F2, "EBMLMaxIDLength", "uint"),
    (0x42F3, "EBMLMaxSizeLength", "uint"),
    (0x4282, "DocType", "string"),
    (0x4287, "DocTypeVersion", "uint"),
    (0x4285, "DocTypeReadVersion", "uint"),
    (0x18538067, "Segment", "master"),
    (0x114D9B74, "SeekHead", "master"),
    (0x4DBB, "Seek", "master"),
    (0x53AB, "SeekID", "binary"),
    (0x53AC, "SeekPosition", "uint"),
    (0x1549A966, "Info", "master"),
    (0x2AD7B1, "TimecodeScale", "uint"),
    (0x4489, "Duration", "float"),
    (0x4D80, "MuxingApp", "utf8"),
    (0x5741, "WritingApp", "utf8"),
    (0x7BA9, "Title", "utf8"),
    (0x1654AE6B, "Tracks", "master"),
    (0xAE, "TrackEntry", "master"),
    (0xD7, "TrackNumber", "uint"),
    (0x73C5, "TrackUID", "uint"),
    (0x83, "TrackType", "uint"),
    (0x86, "CodecID", "string"),
    (0x63A2, "CodecPrivate", "binary"),
    (0x23E383, "DefaultDuration", "uint"),
    (0x536E, "Name", "utf8"),
    (0x22B59C, "Language", "string"),
    (0x9C, "FlagLacing", "uint"),
    (0x258688, "CodecName", "utf8"),
    (0xE0, "Video", "master"),
    (0xB0, "PixelWidth", "uint"),
    (0xBA, "PixelHeight", "uint"),
    (0xE1, "Audio", "master"),
    (0xB5, "SamplingFrequency", "float"),
    (0x9F, "Channels", "uint"),
    (0x6264, "BitDepth", "uint"),
    (0x1F43B675, "Cluster", "master"),
    (0xE7, "Timecode", "uint"),
    (0xA3, "SimpleBlock", "binary"),
    (0xA0, "BlockGroup", "master"),
    (0xA1, "Block", "binary"),
    (0x9B, "BlockDuration", "uint"),
    (0xFB, "ReferenceBlock", "int"),
    (0xAB, "PrevSize", "uint"),
    (0x5854, "SilentTracks", "master"),
    (0x1C53BB6B, "Cues", "master"),
    (0xBB, "CuePoint", "master"),
    (0xB3, "CueTime", "uint"),
    (0xB7, "CueTrackPositions", "master"),
    (0xF7, "CueTrack", "uint"),
    (0xF1, "CueClusterPosition", "uint"),
    (0xF0, "CueRelativePosition", "uint"),
    (0x1043A770, "Chapters", "master"),
    (0x1254C367, "Tags", "master"),
    (0x7373, "Tag", "master"),
    (0x63C0, "Targets", "master"),
    (0x63C5, "TagTrackUID", "uint"),
    (0x67C8, "SimpleTag", "master"),
    (0x45A3, "TagName", "utf8"),
    (0x4487, "TagString", "utf8"),
    (0x447A, "TagLanguage", "string"),
    (0x4484, "TagDefault", "uint"),
    (0x4485, "TagBinary", "binary"),
    (0x1941A469, "Attachments", "master"),
    (0xEC, "Void", "binary"),
    (0xBF, "CRC-32", "binary"),
)

ELEMENTS: dict[int, ElementSpec] = {
    eid: ElementSpec(eid, name, kind) for eid, name, kind in _SPECS
}
ELEMENT_NAMES: dict[int, str] = {eid: spec.name for eid, spec in ELEMENTS.items()}
ELEMENT_TYPES: dict[int, str] = {eid: spec.kind for eid, spec in ELEMENTS.items()}
_UNKNOWN_NAMES: dict[int, ElementSpec] = {}


def spec_for(eid: int) -> ElementSpec:
    spec = ELEMENTS.get(eid)
    if spec is not None:
        return spec
    cached = _UNKNOWN_NAMES.get(eid)
    if cached is None:
        cached = ElementSpec(eid, f"Unknown_0x{eid:X}", "binary")
        _UNKNOWN_NAMES[eid] = cached
    return cached


def hex_id(eid: int) -> str:
    width = max(1, (eid.bit_length() + 7) // 8)
    return "0x" + eid.to_bytes(width, "big").hex().upper()


MASTER_IDS = frozenset(eid for eid, _, kind in _SPECS if kind == "master")
CRC_ID = 0xBF
VOID_ID = 0xEC
SEGMENT_ID = 0x18538067
CLUSTER_ID = 0x1F43B675
EBML_ID = 0x1A45DFA3
TRACK_ENTRY_ID = 0xAE

SEGMENT_CHILDREN = frozenset(
    {
        0x114D9B74,  # SeekHead
        0x1549A966,  # Info
        0x1654AE6B,  # Tracks
        0x1F43B675,  # Cluster
        0x1C53BB6B,  # Cues
        0x1043A770,  # Chapters
        0x1254C367,  # Tags
        0x1941A469,  # Attachments
        VOID_ID,
        CRC_ID,
    }
)

CLUSTER_CHILDREN = frozenset(
    {
        0xE7,  # Timecode
        0xA3,  # SimpleBlock
        0xA0,  # BlockGroup
        0xAB,  # PrevSize
        0x5854,  # SilentTracks
        VOID_ID,
        CRC_ID,
    }
)

TRACK_ENTRY_CHILDREN = frozenset(
    {
        0xD7,
        0x73C5,
        0x83,
        0x86,
        0x63A2,
        0x23E383,
        0x536E,
        0x22B59C,
        0x9C,
        0x258688,
        0xE0,
        0xE1,
        VOID_ID,
        CRC_ID,
    }
)

CHILDREN: dict[int, frozenset[int]] = {
    EBML_ID: frozenset(
        {0x4286, 0x42F7, 0x42F2, 0x42F3, 0x4282, 0x4287, 0x4285, VOID_ID, CRC_ID}
    ),
    SEGMENT_ID: SEGMENT_CHILDREN,
    CLUSTER_ID: CLUSTER_CHILDREN,
    0xA0: frozenset({0xA1, 0x9B, 0xFB, VOID_ID, CRC_ID}),
    0x1654AE6B: frozenset({TRACK_ENTRY_ID, VOID_ID, CRC_ID}),
    TRACK_ENTRY_ID: TRACK_ENTRY_CHILDREN,
    0xE0: frozenset({0xB0, 0xBA, VOID_ID, CRC_ID}),
    0xE1: frozenset({0xB5, 0x9F, 0x6264, VOID_ID, CRC_ID}),
    0x114D9B74: frozenset({0x4DBB, VOID_ID, CRC_ID}),
    0x4DBB: frozenset({0x53AB, 0x53AC, VOID_ID, CRC_ID}),
    0x1549A966: frozenset(
        {0x2AD7B1, 0x4489, 0x4D80, 0x5741, 0x7BA9, VOID_ID, CRC_ID}
    ),
    0x1C53BB6B: frozenset({0xBB, VOID_ID, CRC_ID}),
    0xBB: frozenset({0xB3, 0xB7, VOID_ID, CRC_ID}),
    0xB7: frozenset({0xF7, 0xF1, 0xF0, VOID_ID, CRC_ID}),
    0x1254C367: frozenset({0x7373, VOID_ID, CRC_ID}),
    0x7373: frozenset({0x63C0, 0x67C8, VOID_ID, CRC_ID}),
    0x63C0: frozenset({0x63C5, VOID_ID, CRC_ID}),
    0x67C8: frozenset(
        {0x45A3, 0x4487, 0x447A, 0x4484, 0x4485, 0x67C8, VOID_ID, CRC_ID}
    ),
}
