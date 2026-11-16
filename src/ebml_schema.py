"""Matroska element ID table and schema registry.

Every ID below includes its EBML length-marker bits, exactly as it appears on
the wire. The registry maps ID bytes to :class:`ElementSpec` records used by
the reader to decide between master containers and leaf payloads.
"""

from __future__ import annotations

from ._schema_core import (
    ELEMENT_TYPES,
    MAX_VINT_WIDTH,
    UNKNOWN_SIZE,
    ElementSpec,
    ParseState,
    SchemaError,
    Strictness,
    decode_date,
    decode_float,
    decode_int,
    decode_payload,
    decode_string,
    decode_uint,
    decode_utf8,
    decode_vint,
    encode_float,
    encode_int,
    encode_uint,
    encode_unknown_size,
    encode_vint,
    is_unknown_size,
    vint_mask,
    vint_width,
)
# ── Matroska element IDs ──────────────────────────────────────────────────
# IDs include their length-marker bits; these are the canonical byte strings.

EBML_HEADER = b"\x1a\x45\xdf\xa3"
EBML_VERSION = b"\x42\x86"
EBML_READ_VERSION = b"\x42\xf7"
EBML_MAX_ID_LENGTH = b"\x42\xf2"
EBML_MAX_SIZE_LENGTH = b"\x42\xf3"
DOC_TYPE = b"\x42\x82"
DOC_TYPE_VERSION = b"\x42\x87"
DOC_TYPE_READ_VERSION = b"\x42\x85"

SEGMENT = b"\x18\x53\x80\x67"
SEEK_HEAD = b"\x11\x4d\x9b\x74"
SEEK = b"\x4d\xbb"
SEEK_ID = b"\x53\xab"
SEEK_POSITION = b"\x53\xac"

INFO = b"\x15\x49\xa9\x66"
SEGMENT_UID = b"\x73\xa4"
TIMESTAMP_SCALE = b"\x2a\xd7\xb1"
DURATION = b"\x44\x89"
TITLE = b"\x7b\xa9"
MUXING_APP = b"\x4d\x80"
WRITING_APP = b"\x57\x41"

TRACKS = b"\x16\x54\xae\x6b"
TRACK_ENTRY = b"\xae"
TRACK_NUMBER = b"\xd7"
TRACK_UID = b"\x73\xc5"
TRACK_TYPE = b"\x83"
FLAG_ENABLED = b"\xb9"
FLAG_DEFAULT = b"\x88"
FLAG_FORCED = b"\x55\xaa"
FLAG_LACING = b"\x9c"
DEFAULT_DURATION = b"\x23\xe3\x83"
NAME = b"\x53\x6e"
LANGUAGE = b"\x22\xb5\x9c"
LANGUAGE_BCP47 = b"\x22\xb5\x9d"
CODEC_ID = b"\x86"
CODEC_PRIVATE = b"\x63\xa2"
CODEC_NAME = b"\x25\x86\x88"
CODE_C_DELAY = b"\x56\xaa"
TRACK_TIMESTAMP_SCALE = b"\x23\x31\x4f"

VIDEO = b"\xe0"
PIXEL_WIDTH = b"\xb0"
PIXEL_HEIGHT = b"\xba"
DISPLAY_WIDTH = b"\x54\xb0"
DISPLAY_HEIGHT = b"\x54\xba"

AUDIO = b"\xe1"
SAMPLING_FREQUENCY = b"\xb5"
OUTPUT_SAMPLING_FREQUENCY = b"\x78\xb5"
CHANNELS = b"\x9f"
BIT_DEPTH = b"\x62\x64"

CONTENT_ENCODINGS = b"\x6d\x80"
CONTENT_ENCODING = b"\x62\x40"
CONTENT_ENCODING_ORDER = b"\x50\x31"
CONTENT_ENCODING_SCOPE = b"\x50\x32"
CONTENT_COMPRESSION = b"\x50\x34"
CONTENT_COMP_ALGO = b"\x42\x54"
CONTENT_COMP_SETTINGS = b"\x42\x55"

CLUSTER = b"\x1f\x43\xb6\x75"
TIMESTAMP = b"\xe7"
POSITION = b"\xa7"
PREV_SIZE = b"\xab"
SIMPLE_BLOCK = b"\xa3"
BLOCK_GROUP = b"\xa0"
BLOCK = b"\xa1"
BLOCK_DURATION = b"\x9b"
REFERENCE_BLOCK = b"\xfb"
BLOCK_ADD_ID = b"\xee"
BLOCK_ADDITIONS = b"\x75\xa1"
BLOCK_MORE = b"\xa6"
BLOCK_ADDITIONAL = b"\xa5"
SILENT_TRACKS = b"\x58\x54"
DISCARD_PADDING = b"\x75\xa2"

CUES = b"\x1c\x53\xbb\x6b"
CUE_POINT = b"\xbb"
CUE_TIME = b"\xb3"
CUE_TRACK_POSITIONS = b"\xb7"
CUE_TRACK = b"\xf7"
CUE_CLUSTER_POSITION = b"\xf1"
CUE_RELATIVE_POSITION = b"\xf0"
CUE_DURATION = b"\xb2"

ATTACHMENTS = b"\x19\x41\xa4\x69"
ATTACHED_FILE = b"\x61\xa7"
FILE_DESCRIPTION = b"\x46\x7e"
FILE_NAME = b"\x46\x6e"
FILE_MIME_TYPE = b"\x46\x60"
FILE_DATA = b"\x46\x5c"
FILE_UID = b"\x46\xae"

CHAPTERS = b"\x10\x43\xa7\x70"
TAGS = b"\x12\x54\xc3\x67"
VOID = b"\xec"
CRC32 = b"\xbf"

#: Element name → spec. ``level`` is informational depth (Segment = 1).
SCHEMA: dict[bytes, ElementSpec] = {}


def _register(
    name: str, eid: bytes, etype: str, level: int, parents: tuple[str, ...] = (), multiple: bool = False
) -> ElementSpec:
    spec = ElementSpec(name, eid, etype, level, parents, multiple)
    SCHEMA[eid] = spec
    return spec


# EBML header
_register("EBML", EBML_HEADER, "master", 0)
_register("EBMLVersion", EBML_VERSION, "uint", 1, ("EBML",))
_register("EBMLReadVersion", EBML_READ_VERSION, "uint", 1, ("EBML",))
_register("EBMLMaxIDLength", EBML_MAX_ID_LENGTH, "uint", 1, ("EBML",))
_register("EBMLMaxSizeLength", EBML_MAX_SIZE_LENGTH, "uint", 1, ("EBML",))
_register("DocType", DOC_TYPE, "string", 1, ("EBML",))
_register("DocTypeVersion", DOC_TYPE_VERSION, "uint", 1, ("EBML",))
_register("DocTypeReadVersion", DOC_TYPE_READ_VERSION, "uint", 1, ("EBML",))

# Segment and its children
_register("Segment", SEGMENT, "master", 1)
_register("SeekHead", SEEK_HEAD, "master", 2, ("Segment",), True)
_register("Seek", SEEK, "master", 3, ("SeekHead",), True)
_register("SeekID", SEEK_ID, "binary", 4, ("Seek",))
_register("SeekPosition", SEEK_POSITION, "uint", 4, ("Seek",))
_register("Info", INFO, "master", 2, ("Segment",))
_register("SegmentUID", SEGMENT_UID, "binary", 3, ("Info",))
_register("TimecodeScale", TIMESTAMP_SCALE, "uint", 3, ("Info",))
_register("Duration", DURATION, "float", 3, ("Info",))
_register("Title", TITLE, "utf8", 3, ("Info",))
_register("MuxingApp", MUXING_APP, "utf8", 3, ("Info",))
_register("WritingApp", WRITING_APP, "utf8", 3, ("Info",))

_register("Tracks", TRACKS, "master", 2, ("Segment",))
_register("TrackEntry", TRACK_ENTRY, "master", 3, ("Tracks",), True)
_register("TrackNumber", TRACK_NUMBER, "uint", 4, ("TrackEntry",))
_register("TrackUID", TRACK_UID, "uint", 4, ("TrackEntry",))
_register("TrackType", TRACK_TYPE, "uint", 4, ("TrackEntry",))
_register("FlagEnabled", FLAG_ENABLED, "uint", 4, ("TrackEntry",))
_register("FlagDefault", FLAG_DEFAULT, "uint", 4, ("TrackEntry",))
_register("FlagForced", FLAG_FORCED, "uint", 4, ("TrackEntry",))
_register("FlagLacing", FLAG_LACING, "uint", 4, ("TrackEntry",))
_register("DefaultDuration", DEFAULT_DURATION, "uint", 4, ("TrackEntry",))
_register("Name", NAME, "utf8", 4, ("TrackEntry",))
_register("Language", LANGUAGE, "string", 4, ("TrackEntry",))
_register("LanguageBCP47", LANGUAGE_BCP47, "string", 4, ("TrackEntry",))
_register("CodecID", CODEC_ID, "string", 4, ("TrackEntry",))
_register("CodecPrivate", CODEC_PRIVATE, "binary", 4, ("TrackEntry",))
_register("CodecName", CODEC_NAME, "utf8", 4, ("TrackEntry",))
_register("CodecDelay", CODE_C_DELAY, "uint", 4, ("TrackEntry",))
_register("TrackTimestampScale", TRACK_TIMESTAMP_SCALE, "float", 4, ("TrackEntry",))

_register("Video", VIDEO, "master", 4, ("TrackEntry",))
_register("PixelWidth", PIXEL_WIDTH, "uint", 5, ("Video",))
_register("PixelHeight", PIXEL_HEIGHT, "uint", 5, ("Video",))
_register("DisplayWidth", DISPLAY_WIDTH, "uint", 5, ("Video",))
_register("DisplayHeight", DISPLAY_HEIGHT, "uint", 5, ("Video",))

_register("Audio", AUDIO, "master", 4, ("TrackEntry",))
_register("SamplingFrequency", SAMPLING_FREQUENCY, "float", 5, ("Audio",))
_register("OutputSamplingFrequency", OUTPUT_SAMPLING_FREQUENCY, "float", 5, ("Audio",))
_register("Channels", CHANNELS, "uint", 5, ("Audio",))
_register("BitDepth", BIT_DEPTH, "uint", 5, ("Audio",))

_register("ContentEncodings", CONTENT_ENCODINGS, "master", 4, ("TrackEntry",))
_register("ContentEncoding", CONTENT_ENCODING, "master", 5, ("ContentEncodings",), True)
_register("ContentEncodingOrder", CONTENT_ENCODING_ORDER, "uint", 6, ("ContentEncoding",))
_register("ContentEncodingScope", CONTENT_ENCODING_SCOPE, "uint", 6, ("ContentEncoding",))
_register("ContentCompression", CONTENT_COMPRESSION, "master", 6, ("ContentEncoding",))
_register("ContentCompAlgo", CONTENT_COMP_ALGO, "uint", 7, ("ContentCompression",))
_register("ContentCompSettings", CONTENT_COMP_SETTINGS, "binary", 7, ("ContentCompression",))

_register("Cluster", CLUSTER, "master", 2, ("Segment",), True)
_register("Timestamp", TIMESTAMP, "uint", 3, ("Cluster",))
_register("Position", POSITION, "uint", 3, ("Cluster",))
_register("PrevSize", PREV_SIZE, "uint", 3, ("Cluster",))
_register("SimpleBlock", SIMPLE_BLOCK, "binary", 3, ("Cluster",), True)
_register("BlockGroup", BLOCK_GROUP, "master", 3, ("Cluster",), True)
_register("Block", BLOCK, "binary", 4, ("BlockGroup",))
_register("BlockDuration", BLOCK_DURATION, "uint", 4, ("BlockGroup",))
_register("ReferenceBlock", REFERENCE_BLOCK, "int", 4, ("BlockGroup",), True)
_register("BlockAddID", BLOCK_ADD_ID, "uint", 4, ("BlockAdditions",))
_register("BlockAdditions", BLOCK_ADDITIONS, "master", 4, ("BlockGroup",))
_register("BlockMore", BLOCK_MORE, "master", 5, ("BlockAdditions",), True)
_register("BlockAdditional", BLOCK_ADDITIONAL, "binary", 5, ("BlockMore",))
_register("SilentTracks", SILENT_TRACKS, "master", 3, ("Cluster",))
_register("DiscardPadding", DISCARD_PADDING, "int", 4, ("BlockGroup",))

_register("Cues", CUES, "master", 2, ("Segment",))
_register("CuePoint", CUE_POINT, "master", 3, ("Cues",), True)
_register("CueTime", CUE_TIME, "uint", 4, ("CuePoint",))
_register("CueTrackPositions", CUE_TRACK_POSITIONS, "master", 4, ("CuePoint",), True)
_register("CueTrack", CUE_TRACK, "uint", 5, ("CueTrackPositions",))
_register("CueClusterPosition", CUE_CLUSTER_POSITION, "uint", 5, ("CueTrackPositions",))
_register("CueRelativePosition", CUE_RELATIVE_POSITION, "uint", 5, ("CueTrackPositions",))
_register("CueDuration", CUE_DURATION, "uint", 5, ("CueTrackPositions",))

_register("Attachments", ATTACHMENTS, "master", 2, ("Segment",))
_register("AttachedFile", ATTACHED_FILE, "master", 3, ("Attachments",), True)
_register("FileDescription", FILE_DESCRIPTION, "utf8", 4, ("AttachedFile",))
_register("FileName", FILE_NAME, "utf8", 4, ("AttachedFile",))
_register("FileMimeType", FILE_MIME_TYPE, "string", 4, ("AttachedFile",))
_register("FileData", FILE_DATA, "binary", 4, ("AttachedFile",))
_register("FileUID", FILE_UID, "uint", 4, ("AttachedFile",))

_register("Chapters", CHAPTERS, "master", 2, ("Segment",))
_register("Tags", TAGS, "master", 2, ("Segment",))

_register("CRC-32", CRC32, "binary", 0)
_register("Void", VOID, "binary", 0)

#: Master elements we descend into; everything else is read flat.
MASTER_IDS = frozenset(eid for eid, spec in SCHEMA.items() if spec.type == "master")

#: Elements permitted to carry an unknown size (Matroska level-1/2 containers).
UNKNOWN_SIZE_ALLOWED = frozenset({SEGMENT, CLUSTER})

#: Track types: 1 video, 2 audio, 17 subtitle.
TRACK_TYPE_VIDEO = 1
TRACK_TYPE_AUDIO = 2
TRACK_TYPE_SUBTITLE = 17

TRACK_TYPE_NAMES = {1: "video", 2: "audio", 17: "subtitle", 0: "unknown"}


def spec_for(element_id: bytes) -> ElementSpec | None:
    """Look up a schema entry; unknown IDs are legal in EBML (Void/CRC aside)."""
    return SCHEMA.get(element_id)


def name_for(element_id: bytes) -> str:
    spec = SCHEMA.get(element_id)
    return spec.name if spec else f"Unknown:{element_id.hex()}"


def is_master(element_id: bytes) -> bool:
    return element_id in MASTER_IDS
