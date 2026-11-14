"""Element IDs and containment metadata used by the streaming reader."""

from __future__ import annotations

EBML = 0x1A45DFA3
SEGMENT = 0x18538067
CLUSTER = 0x1F43B675
CUES = 0x1C53BB6B
CHAPTERS = 0x1043A770
TAGS = 0x1254C367
INFO = 0x1549A966
TRACKS = 0x1654AE6B
ATTACHMENTS = 0x1941A469
SEEK_HEAD = 0x114D9B74
VOID = 0xEC
CRC32 = 0xBF

NAMES: dict[int, str] = {
    0x1A45DFA3: "EBML", 0x4286: "EBMLVersion", 0x42F7: "EBMLReadVersion",
    0x42F2: "EBMLMaxIDLength", 0x42F3: "EBMLMaxSizeLength", 0x4282: "DocType",
    0x4287: "DocTypeVersion", 0x4285: "DocTypeReadVersion", 0x4281: "DocTypeExtension",
    0x18538067: "Segment", 0x114D9B74: "SeekHead", 0x4DBB: "Seek",
    0x53AB: "SeekID", 0x53AC: "SeekPosition", 0x1549A966: "Info",
    0x2AD7B1: "TimestampScale", 0x4489: "Duration", 0x4D80: "MuxingApp",
    0x5741: "WritingApp", 0x7BA9: "Title", 0x4461: "DateUTC",
    0x1654AE6B: "Tracks", 0xAE: "TrackEntry", 0xD7: "TrackNumber",
    0x73C5: "TrackUID", 0x83: "TrackType", 0x9C: "FlagLacing",
    0x23E383: "DefaultDuration", 0x536E: "Name", 0x22B59C: "Language",
    0x22B59D: "LanguageIETF", 0x86: "CodecID", 0x63A2: "CodecPrivate",
    0x258688: "CodecName", 0x88: "FlagDefault", 0x55AA: "FlagForced",
    0x55AB: "FlagHearingImpaired", 0x55AC: "FlagVisualImpaired",
    0x55AD: "FlagTextDescriptions", 0x55AE: "FlagOriginal",
    0x55AF: "FlagCommentary", 0x9A: "FlagEnabled", 0xB9: "FlagEnabled",
    0xE0: "Video", 0xB0: "PixelWidth", 0xBA: "PixelHeight",
    0x54B0: "DisplayWidth", 0x54BA: "DisplayHeight", 0x54B2: "DisplayUnit",
    0xE1: "Audio", 0xB5: "SamplingFrequency", 0x9F: "Channels",
    0x6264: "BitDepth", 0x1F43B675: "Cluster", 0xE7: "Timestamp",
    0xA7: "Position", 0xAB: "PrevSize", 0xA3: "SimpleBlock",
    0xA0: "BlockGroup", 0xA1: "Block", 0x9B: "BlockDuration",
    0xFB: "ReferenceBlock", 0x75A1: "BlockAdditions", 0x1C53BB6B: "Cues",
    0xBB: "CuePoint", 0xB3: "CueTime", 0xB7: "CueTrackPositions",
    0xF7: "CueTrack", 0xF1: "CueClusterPosition", 0xF0: "CueRelativePosition",
    0xDB: "CueDuration", 0x1043A770: "Chapters", 0x45B9: "EditionEntry",
    0xB6: "ChapterAtom", 0x73C4: "ChapterUID", 0x91: "ChapterTimeStart",
    0x92: "ChapterTimeEnd", 0x80: "ChapterDisplay", 0x85: "ChapString",
    0x437C: "ChapLanguage", 0x437D: "ChapLanguageIETF", 0x1254C367: "Tags",
    0x7373: "Tag", 0x63C0: "Targets", 0x68CA: "TargetTypeValue",
    0x63C5: "TagTrackUID", 0x67C8: "SimpleTag", 0x45A3: "TagName",
    0x4487: "TagString", 0x447A: "TagLanguage", 0x4484: "TagDefault",
    0x1941A469: "Attachments", 0x61A7: "AttachedFile", 0x467E: "FileDescription",
    0x466E: "FileName", 0x4660: "FileMimeType", 0x465C: "FileData",
    0x46AE: "FileUID", 0xEC: "Void", 0xBF: "CRC-32",
}

CONTAINERS: frozenset[int] = frozenset({
    EBML, SEGMENT, SEEK_HEAD, 0x4DBB, INFO, TRACKS, 0xAE, 0xE0, 0xE1,
    CLUSTER, 0xA0, 0x75A1, CUES, 0xBB, 0xB7, CHAPTERS, 0x45B9, 0xB6,
    0x80, TAGS, 0x7373, 0x63C0, 0x67C8, ATTACHMENTS, 0x61A7,
})

SEGMENT_CHILDREN: frozenset[int] = frozenset({
    SEEK_HEAD, INFO, TRACKS, CLUSTER, CUES, CHAPTERS, TAGS, ATTACHMENTS,
    VOID, CRC32,
})

CHAPTER_CHILDREN: frozenset[int] = frozenset({0xB6, 0x45B9, 0x80, VOID, CRC32})

ID_WIDTHS = {element_id: (element_id.bit_length() + 7) // 8 for element_id in NAMES}


def element_name(element_id: int) -> str:
    return NAMES.get(element_id, f"0x{element_id:X}")


def is_container(element_id: int) -> bool:
    return element_id in CONTAINERS

