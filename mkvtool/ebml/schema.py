"""Matroska element IDs, primitive types, and parent-child constraints."""

from __future__ import annotations

from dataclasses import dataclass
from functools import lru_cache

MASTER = "master"
UINT = "uint"
INT = "int"
FLOAT = "float"
STRING = "string"
UTF8 = "utf8"
BINARY = "binary"


@dataclass(frozen=True)
class ElementSpec:
    element_id: int
    name: str
    type: str
    children: frozenset[int] = frozenset()


def _spec(element_id: int, name: str, kind: str, children: set[int] | None = None) -> ElementSpec:
    return ElementSpec(element_id, name, kind, frozenset(children or ()))


EBML_ID = 0x1A45DFA3
SEGMENT_ID = 0x18538067
CLUSTER_ID = 0x1F43B675
TRACKS_ID = 0x1654AE6B
TRACK_ENTRY_ID = 0xAE
INFO_ID = 0x1549A966
CUES_ID = 0x1C53BB6B
TAGS_ID = 0x1254C367
SIMPLE_BLOCK_ID = 0xA3
BLOCK_GROUP_ID = 0xA0
BLOCK_ID = 0xA1
VOID_ID = 0xEC

_SPECS = [
    _spec(EBML_ID, "EBML", MASTER, {0x4286, 0x42F7, 0x42F2, 0x42F3, 0x4282, 0x4287, 0x4285}),
    _spec(0x4286, "EBMLVersion", UINT),
    _spec(0x42F7, "EBMLReadVersion", UINT),
    _spec(0x42F2, "EBMLMaxIDLength", UINT),
    _spec(0x42F3, "EBMLMaxSizeLength", UINT),
    _spec(0x4282, "DocType", STRING),
    _spec(0x4287, "DocTypeVersion", UINT),
    _spec(0x4285, "DocTypeReadVersion", UINT),
    _spec(SEGMENT_ID, "Segment", MASTER, {
        0x114D9B74, INFO_ID, TRACKS_ID, CLUSTER_ID, CUES_ID, 0x1043A770, TAGS_ID, 0x1941A469,
    }),
    _spec(0x114D9B74, "SeekHead", MASTER, {0x4DBB}),
    _spec(0x4DBB, "Seek", MASTER, {0x53AB, 0x53AC}),
    _spec(0x53AB, "SeekID", BINARY),
    _spec(0x53AC, "SeekPosition", UINT),
    _spec(INFO_ID, "Info", MASTER, {0x2AD7B1, 0x4489, 0x4D80, 0x5741, 0x73A4, 0x7BA9}),
    _spec(0x2AD7B1, "TimecodeScale", UINT),
    _spec(0x4489, "Duration", FLOAT),
    _spec(0x4D80, "MuxingApp", UTF8),
    _spec(0x5741, "WritingApp", UTF8),
    _spec(0x73A4, "SegmentUID", BINARY),
    _spec(0x7BA9, "Title", UTF8),
    _spec(TRACKS_ID, "Tracks", MASTER, {TRACK_ENTRY_ID}),
    _spec(TRACK_ENTRY_ID, "TrackEntry", MASTER, {
        0xD7, 0x73C5, 0x83, 0x88, 0x9C, 0x86, 0x63A2, 0x23E383, 0x22B59C, 0x22B59D,
        0xE0, 0xE1, 0xB9, 0x6DE7,
    }),
    _spec(0xD7, "TrackNumber", UINT),
    _spec(0x73C5, "TrackUID", UINT),
    _spec(0x83, "TrackType", UINT),
    _spec(0x88, "FlagDefault", UINT),
    _spec(0x9C, "FlagLacing", UINT),
    _spec(0x86, "CodecID", STRING),
    _spec(0x63A2, "CodecPrivate", BINARY),
    _spec(0x23E383, "DefaultDuration", UINT),
    _spec(0x22B59C, "Language", STRING),
    _spec(0x22B59D, "LanguageIETF", STRING),
    _spec(0xB9, "FlagEnabled", UINT),
    _spec(0x6DE7, "MinCache", UINT),
    _spec(0xE0, "Video", MASTER, {
        0xB0, 0xBA, 0x54B0, 0x54BA, 0x55B0, 0x9A,
    }),
    _spec(0xB0, "PixelWidth", UINT),
    _spec(0xBA, "PixelHeight", UINT),
    _spec(0x54B0, "DisplayWidth", UINT),
    _spec(0x54BA, "DisplayHeight", UINT),
    _spec(0x9A, "FlagInterlaced", UINT),
    _spec(0x55B0, "Colour", MASTER, {0x55B1, 0x55B2, 0x55B3, 0x55B4, 0x55B5, 0x55B6}),
    _spec(0x55B1, "MatrixCoefficients", UINT),
    _spec(0x55B2, "BitsPerChannel", UINT),
    _spec(0x55B3, "ChromaSubsamplingHorz", UINT),
    _spec(0x55B4, "ChromaSubsamplingVert", UINT),
    _spec(0x55B5, "CbSubsamplingHorz", UINT),
    _spec(0x55B6, "CbSubsamplingVert", UINT),
    _spec(0xE1, "Audio", MASTER, {0xB5, 0x78B5, 0x9F, 0x6264}),
    _spec(0xB5, "SamplingFrequency", FLOAT),
    _spec(0x78B5, "OutputSamplingFrequency", FLOAT),
    _spec(0x9F, "Channels", UINT),
    _spec(0x6264, "BitDepth", UINT),
    _spec(CLUSTER_ID, "Cluster", MASTER, {0xE7, 0xAB, SIMPLE_BLOCK_ID, BLOCK_GROUP_ID}),
    _spec(0xE7, "Timecode", UINT),
    _spec(0xAB, "PrevSize", UINT),
    _spec(SIMPLE_BLOCK_ID, "SimpleBlock", BINARY),
    _spec(BLOCK_GROUP_ID, "BlockGroup", MASTER, {
        BLOCK_ID, 0x9B, 0xFB, 0x75A2, 0x75A1,
    }),
    _spec(BLOCK_ID, "Block", BINARY),
    _spec(0x9B, "BlockDuration", UINT),
    _spec(0xFB, "ReferenceBlock", INT),
    _spec(0x75A2, "DiscardPadding", INT),
    _spec(0x75A1, "BlockAdditions", MASTER, {0xA6}),
    _spec(0xA6, "BlockMore", MASTER, {0xA5, 0xEE}),
    _spec(0xA5, "BlockAdditional", BINARY),
    _spec(0xEE, "BlockAddID", UINT),
    _spec(CUES_ID, "Cues", MASTER, {0xBB}),
    _spec(0xBB, "CuePoint", MASTER, {0xB3, 0xB7}),
    _spec(0xB3, "CueTime", UINT),
    _spec(0xB7, "CueTrackPositions", MASTER, {0xF7, 0xF1, 0xF0, 0x5378, 0xB2}),
    _spec(0xF7, "CueTrack", UINT),
    _spec(0xF1, "CueClusterPosition", UINT),
    _spec(0xF0, "CueRelativePosition", UINT),
    _spec(0x5378, "CueBlockNumber", UINT),
    _spec(0xB2, "CueDuration", UINT),
    _spec(TAGS_ID, "Tags", MASTER, {0x7373}),
    _spec(0x7373, "Tag", MASTER, {0x63C0, 0x67C8}),
    _spec(0x63C0, "Targets", MASTER, {0x68CA, 0x63C5}),
    _spec(0x68CA, "TargetTypeValue", UINT),
    _spec(0x63C5, "TagTrackUID", UINT),
    _spec(0x67C8, "SimpleTag", MASTER, {0x45A3, 0x4487, 0x4485, 0x447A, 0x67C8}),
    _spec(0x45A3, "TagName", UTF8),
    _spec(0x4487, "TagString", UTF8),
    _spec(0x4485, "TagBinary", BINARY),
    _spec(0x447A, "TagLanguage", STRING),
    _spec(VOID_ID, "Void", BINARY),
    _spec(0xBF, "CRC-32", BINARY),
]

SPECS = {spec.element_id: spec for spec in _SPECS}
MASTER_IDS = frozenset(spec.element_id for spec in _SPECS if spec.type == MASTER)


@lru_cache(maxsize=256)
def get_spec(element_id: int) -> ElementSpec:
    known = SPECS.get(element_id)
    if known is not None:
        return known
    return ElementSpec(
        element_id=element_id,
        name=f"Unknown(0x{element_id:X})",
        type=BINARY,
    )


def is_master(element_id: int) -> bool:
    return SPECS.get(element_id, ElementSpec(0, "", BINARY)).type == MASTER


def can_contain(parent_id: int, child_id: int) -> bool:
    """Unknown/vendor IDs stay inside their current parent and size-delimit."""
    parent = SPECS.get(parent_id)
    if parent is None or not parent.children:
        return False
    if child_id not in SPECS:
        return True
    return child_id in parent.children


def element_name(element_id: int) -> str:
    return get_spec(element_id).name
