"""Matroska EBML schema: element ID -> name/type mapping."""

from dataclasses import dataclass


@dataclass(frozen=True)
class ElementSpec:
    id: int
    name: str
    type: str  # master|uint|int|float|string|utf8|binary|date


_SPECS = [
    (0x1A45DFA3, "EBML", "master"),
    (0x4286, "EBMLVersion", "uint"),
    (0x42F7, "EBMLReadVersion", "uint"),
    (0x42F2, "EBMLMaxIDLength", "uint"),
    (0x42F3, "EBMLMaxSizeLength", "uint"),
    (0x4282, "DocType", "string"),
    (0x4287, "DocTypeVersion", "uint"),
    (0x4285, "DocTypeReadVersion", "uint"),
    (0xEC, "Void", "binary"),
    (0xBF, "CRC-32", "binary"),
    (0x18538067, "Segment", "master"),
    (0x114D9B74, "SeekHead", "master"),
    (0x4DBB, "Seek", "master"),
    (0x53AB, "SeekID", "binary"),
    (0x53AC, "SeekPosition", "uint"),
    (0x1549A966, "Info", "master"),
    (0x73A4, "SegmentUUID", "binary"),
    (0x2AD7B1, "TimecodeScale", "uint"),
    (0x4489, "Duration", "float"),
    (0x4461, "DateUTC", "date"),
    (0x7BA9, "Title", "utf8"),
    (0x4D80, "MuxingApp", "utf8"),
    (0x5741, "WritingApp", "utf8"),
    (0x1654AE6B, "Tracks", "master"),
    (0xAE, "TrackEntry", "master"),
    (0xD7, "TrackNumber", "uint"),
    (0x73C5, "TrackUID", "uint"),
    (0x83, "TrackType", "uint"),
    (0xB9, "FlagEnabled", "uint"),
    (0x88, "FlagDefault", "uint"),
    (0x55AA, "FlagForced", "uint"),
    (0x9C, "FlagLacing", "uint"),
    (0x536E, "Name", "utf8"),
    (0x22B59C, "Language", "string"),
    (0x86, "CodecID", "string"),
    (0x63A2, "CodecPrivate", "binary"),
    (0x23E383, "DefaultDuration", "uint"),
    (0x56AA, "CodecDelay", "uint"),
    (0x56BB, "SeekPreRoll", "uint"),
    (0xE0, "Video", "master"),
    (0xB0, "PixelWidth", "uint"),
    (0xBA, "PixelHeight", "uint"),
    (0x54B0, "DisplayWidth", "uint"),
    (0x54BA, "DisplayHeight", "uint"),
    (0x2383E3, "FrameRate", "float"),
    (0xE1, "Audio", "master"),
    (0xB5, "SamplingFrequency", "float"),
    (0x78B5, "OutputSamplingFrequency", "float"),
    (0x9F, "Channels", "uint"),
    (0x6264, "BitDepth", "uint"),
    (0x1F43B675, "Cluster", "master"),
    (0xE7, "Timecode", "uint"),
    (0xA7, "Position", "uint"),
    (0xAB, "PrevSize", "uint"),
    (0xA3, "SimpleBlock", "binary"),
    (0xA0, "BlockGroup", "master"),
    (0xA1, "Block", "binary"),
    (0x9B, "BlockDuration", "uint"),
    (0xFB, "ReferenceBlock", "int"),
    (0x1C53BB6B, "Cues", "master"),
    (0xBB, "CuePoint", "master"),
    (0xB3, "CueTime", "uint"),
    (0xB7, "CueTrackPositions", "master"),
    (0xF7, "CueTrack", "uint"),
    (0xF1, "CueClusterPosition", "uint"),
    (0x5378, "CueBlockNumber", "uint"),
    (0x1254C367, "Tags", "master"),
    (0x7373, "Tag", "master"),
    (0x63C0, "Targets", "master"),
    (0x67C8, "SimpleTag", "master"),
    (0x45A3, "TagName", "utf8"),
    (0x447A, "TagLanguage", "string"),
    (0x4487, "TagString", "utf8"),
    (0x1043A770, "Chapters", "master"),
    (0x1941A469, "Attachments", "master"),
]

SCHEMA = {i: ElementSpec(i, n, t) for i, n, t in _SPECS}
MASTER_IDS = {s.id for s in SCHEMA.values() if s.type == "master"}

# Level-1 elements (direct children of Segment).  An unknown-length
# Cluster ends when one of these IDs appears at its child level.
TOP_LEVEL_IDS = {
    0x114D9B74, 0x1549A966, 0x1654AE6B, 0x1F43B675,
    0x1C53BB6B, 0x1254C367, 0x1043A770, 0x1941A469,
}

EBML_ID = 0x1A45DFA3
SEGMENT_ID = 0x18538067
INFO_ID = 0x1549A966
TRACKS_ID = 0x1654AE6B
TRACK_ENTRY_ID = 0xAE
VIDEO_ID = 0xE0
AUDIO_ID = 0xE1
CLUSTER_ID = 0x1F43B675
TIMECODE_ID = 0xE7
SIMPLE_BLOCK_ID = 0xA3
BLOCK_GROUP_ID = 0xA0
BLOCK_ID = 0xA1
REFERENCE_BLOCK_ID = 0xFB
CUES_ID = 0x1C53BB6B
CUE_CLUSTER_POSITION_ID = 0xF1
TIMECODE_SCALE_ID = 0x2AD7B1
DURATION_ID = 0x4489


def spec_for(element_id):
    """Look up an element spec; unknown IDs decode as opaque binary."""
    spec = SCHEMA.get(element_id)
    if spec is None:
        spec = ElementSpec(element_id, "Unknown-0x%X" % element_id, "binary")
    return spec
