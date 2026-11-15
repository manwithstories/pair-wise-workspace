"""Static EBML element table for the Matroska subset mkvkit understands.

Maps element IDs to ``(name, type)`` where type is one of ``master``,
``uint``, ``int``, ``float``, ``string``, ``utf8``, ``binary``, ``date``.
"""
from __future__ import annotations

MASTER = "master"
UINT = "uint"
INT = "int"
FLOAT = "float"
STRING = "string"
UTF8 = "utf8"
BINARY = "binary"
DATE = "date"

# Frequently referenced element IDs.
ID_EBML = 0x1A45DFA3
ID_DOCTYPE = 0x4282
ID_VOID = 0xEC
ID_CRC32 = 0xBF
ID_SEGMENT = 0x18538067
ID_INFO = 0x1549A966
ID_TIMESTAMP_SCALE = 0x2AD7B1
ID_TRACKS = 0x1654AE6B
ID_TRACK_ENTRY = 0xAE
ID_TRACK_NUMBER = 0xD7
ID_TRACK_TYPE = 0x83
ID_CODEC_ID = 0x86
ID_CODEC_PRIVATE = 0x63A2
ID_DEFAULT_DURATION = 0x23E383
ID_LANGUAGE = 0x22B59C
ID_VIDEO = 0xE0
ID_PIXEL_WIDTH = 0xB0
ID_PIXEL_HEIGHT = 0xBA
ID_AUDIO = 0xE1
ID_SAMPLING_FREQUENCY = 0xB5
ID_CHANNELS = 0x9F
ID_CLUSTER = 0x1F43B675
ID_TIMESTAMP = 0xE7
ID_BLOCK_GROUP = 0xA0
ID_BLOCK = 0xA1
ID_SIMPLE_BLOCK = 0xA3
ID_BLOCK_DURATION = 0x9B
ID_REFERENCE_BLOCK = 0xFB
ID_CUES = 0x1C53BB6B

ELEMENTS = {
    0x1A45DFA3: ("EBML", MASTER),
    0x4286: ("EBMLVersion", UINT),
    0x42F7: ("EBMLReadVersion", UINT),
    0x42F2: ("EBMLMaxIDLength", UINT),
    0x42F3: ("EBMLMaxSizeLength", UINT),
    0x4282: ("DocType", STRING),
    0x4287: ("DocTypeVersion", UINT),
    0x4285: ("DocTypeReadVersion", UINT),
    0xEC: ("Void", BINARY),
    0xBF: ("CRC-32", BINARY),
    0x18538067: ("Segment", MASTER),
    0x114D9B74: ("SeekHead", MASTER),
    0x4DBB: ("Seek", MASTER),
    0x53AB: ("SeekID", BINARY),
    0x53AC: ("SeekPosition", UINT),
    0x1549A966: ("Info", MASTER),
    0x2AD7B1: ("TimestampScale", UINT),
    0x4489: ("Duration", FLOAT),
    0x4D80: ("MuxingApp", UTF8),
    0x5741: ("WritingApp", UTF8),
    0x4461: ("DateUTC", DATE),
    0x1654AE6B: ("Tracks", MASTER),
    0xAE: ("TrackEntry", MASTER),
    0xD7: ("TrackNumber", UINT),
    0x73C5: ("TrackUID", UINT),
    0x83: ("TrackType", UINT),
    0xB9: ("FlagEnabled", UINT),
    0x88: ("FlagDefault", UINT),
    0x9C: ("FlagLacing", UINT),
    0x86: ("CodecID", STRING),
    0x258688: ("CodecName", UTF8),
    0x23E383: ("DefaultDuration", UINT),
    0x22B59C: ("Language", STRING),
    0x63A2: ("CodecPrivate", BINARY),
    0xE0: ("Video", MASTER),
    0xB0: ("PixelWidth", UINT),
    0xBA: ("PixelHeight", UINT),
    0x54B0: ("DisplayWidth", UINT),
    0x54BA: ("DisplayHeight", UINT),
    0xE1: ("Audio", MASTER),
    0xB5: ("SamplingFrequency", FLOAT),
    0x9F: ("Channels", UINT),
    0x6264: ("BitDepth", UINT),
    0x1F43B675: ("Cluster", MASTER),
    0xE7: ("Timestamp", UINT),
    0xA7: ("Position", UINT),
    0xAB: ("PrevSize", UINT),
    0xA0: ("BlockGroup", MASTER),
    0xA1: ("Block", BINARY),
    0xA2: ("BlockVirtual", BINARY),
    0x75A1: ("BlockAdditions", MASTER),
    0xA6: ("BlockMore", MASTER),
    0xEE: ("BlockAddID", UINT),
    0xA5: ("BlockAdditional", BINARY),
    0x9B: ("BlockDuration", UINT),
    0xFA: ("ReferencePriority", UINT),
    0xFB: ("ReferenceBlock", INT),
    0xFD: ("ReferenceVirtual", INT),
    0xA4: ("CodecState", BINARY),
    0x75A2: ("DiscardPadding", INT),
    0x8E: ("Slices", MASTER),
    0xC8: ("ReferenceFrame", MASTER),
    0xA3: ("SimpleBlock", BINARY),
    0x1C53BB6B: ("Cues", MASTER),
    0xBB: ("CuePoint", MASTER),
    0xB3: ("CueTime", UINT),
    0xB7: ("CueTrackPositions", MASTER),
    0xF7: ("CueTrack", UINT),
    0xF1: ("CueClusterPosition", UINT),
    0x1254C367: ("Tags", MASTER),
    0x7373: ("Tag", MASTER),
    0x67C8: ("SimpleTag", MASTER),
    0x45A3: ("TagName", UTF8),
    0x4487: ("TagString", UTF8),
    0x1043A770: ("Chapters", MASTER),
    0x1941A469: ("Attachments", MASTER),
}

# IDs that may appear as direct children of Segment; an unknown-size
# Cluster ends when one of these (or EOF) is encountered.
SEGMENT_LEVEL_IDS = frozenset(
    (
        0x1549A966, 0x1654AE6B, 0x1F43B675, 0x1C53BB6B, 0x114D9B74,
        0x1254C367, 0x1043A770, 0x1941A469,
    )
)
