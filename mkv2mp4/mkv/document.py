"""Semantic MKV layer: assembles Info, Tracks, Clusters, Blocks, Cues.

Parsing is streaming: element headers are consumed as a flat walk and
only small leaf payloads (Info/Tracks fields, block headers) are read.
Media sample payloads are referenced by (offset, size) into the source
file and copied lazily by the muxer, keeping resident memory bounded
regardless of file size.
"""

import os
import struct
from dataclasses import dataclass

from ..ebml import schema
from ..ebml.reader import ElementReader
from ..ebml.vint import vint_from_bytes, decode_signed
from ..errors import EbmlError

TRACK_TYPE_NAMES = {1: "video", 2: "audio"}


@dataclass
class Track:
    number: int = 0
    uid: int = 0
    type: int = 0
    codec_id: str = ""
    codec_private: bytes = b""
    name: str = ""
    language: str = "und"
    flag_default: bool = True
    default_duration_ns: int = 0
    width: int = 0
    height: int = 0
    sample_rate: float = 0.0
    channels: int = 0

    @property
    def type_name(self):
        return TRACK_TYPE_NAMES.get(self.type, "other")


@dataclass
class Sample:
    track_number: int
    pts_ns: int          # absolute presentation time in nanoseconds
    keyframe: bool
    offset: int          # absolute offset of payload in the source file
    size: int
    cluster_index: int


@dataclass
class Cluster:
    index: int
    offset: int          # absolute offset of the Cluster element
    timecode: int = 0    # in TimecodeScale units


def _leaf_value(reader, el):
    data = reader.read_data(el)
    if el.type == "uint":
        return int.from_bytes(data, "big")
    if el.type == "int":
        return decode_signed(data)
    if el.type == "float":
        if len(data) == 4:
            return struct.unpack(">f", data)[0]
        if len(data) == 8:
            return struct.unpack(">d", data)[0]
        raise EbmlError("bad float element size", el.offset)
    if el.type in ("string", "utf8"):
        return data.decode("utf-8", "replace").rstrip("\x00")
    return data


class MkvDocument:
    """Parsed Matroska document (semantics on top of ebml.reader)."""

    def __init__(self):
        self.path = ""
        self.file_size = 0
        self.timecode_scale = 1_000_000
        self.duration = None
        self.tracks = []
        self.samples = []
        self.clusters = []
        self.cues_present = False
        self.cue_positions = []
        self.segment_data_offset = 0
        self.stats = {"element_count": 0, "max_depth": 0,
                      "unknown_sized": []}

    @property
    def cues_status(self):
        if not self.cues_present:
            return "missing"
        actual = {c.offset - self.segment_data_offset
                  for c in self.clusters}
        if self.cue_positions and all(p in actual
                                      for p in self.cue_positions):
            return "ok"
        return "stale"

    @property
    def duration_ms(self):
        if self.duration is None:
            return None
        return round(self.duration * self.timecode_scale / 1e6, 3)

    def track_by_number(self, number):
        for t in self.tracks:
            if t.number == number:
                return t
        return None

    @classmethod
    def parse(cls, path, light=False):
        """Parse a Matroska file.

        ``light=True`` collects element statistics and Info/Tracks only
        (no sample table) - used by the streaming ``probe`` command.
        """
        fh = open(path, "rb")
        try:
            size = os.fstat(fh.fileno()).st_size
            doc = cls()
            doc.path = str(path)
            doc.file_size = size
            reader = ElementReader(fh, size)
            stack = []
            cluster = None
            bg_has_reference = False
            for el in reader.walk():
                st = doc.stats
                st["element_count"] += 1
                if el.depth > st["max_depth"]:
                    st["max_depth"] = el.depth
                if el.size is None:
                    st["unknown_sized"].append({
                        "offset": el.offset,
                        "id": "0x%X" % el.id,
                        "name": el.name})
                stack = stack[:el.depth - 1]
                parent_id = stack[-1].id if stack else 0
                in_track = any(e.id == schema.TRACK_ENTRY_ID for e in stack)
                if el.id == schema.SEGMENT_ID:
                    doc.segment_data_offset = el.data_offset
                elif parent_id == schema.INFO_ID:
                    _apply_info(doc, reader, el)
                elif (el.id == schema.TRACK_ENTRY_ID
                      and parent_id == schema.TRACKS_ID):
                    doc.tracks.append(Track())
                elif in_track and doc.tracks:
                    _apply_track(doc.tracks[-1], reader, el, stack)
                elif el.id == schema.CLUSTER_ID:
                    cluster = Cluster(len(doc.clusters), el.offset)
                    doc.clusters.append(cluster)
                elif (el.id == schema.TIMECODE_ID
                      and parent_id == schema.CLUSTER_ID):
                    cluster.timecode = _leaf_value(reader, el)
                elif (el.id == schema.SIMPLE_BLOCK_ID
                      and parent_id == schema.CLUSTER_ID and not light):
                    _parse_block(doc, reader, el, cluster, True, True)
                elif el.id == schema.BLOCK_GROUP_ID:
                    bg_has_reference = False
                elif (el.id == schema.REFERENCE_BLOCK_ID
                      and parent_id == schema.BLOCK_GROUP_ID):
                    bg_has_reference = True
                elif (el.id == schema.BLOCK_ID
                      and parent_id == schema.BLOCK_GROUP_ID and not light):
                    _parse_block(doc, reader, el, cluster, False,
                                 not bg_has_reference)
                elif el.id == schema.CUES_ID:
                    doc.cues_present = True
                elif el.id == schema.CUE_CLUSTER_POSITION_ID:
                    doc.cue_positions.append(_leaf_value(reader, el))
                if el.is_master:
                    stack.append(el)
            return doc
        finally:
            fh.close()


def _apply_info(doc, reader, el):
    if el.id == schema.TIMECODE_SCALE_ID:
        doc.timecode_scale = _leaf_value(reader, el)
    elif el.id == schema.DURATION_ID:
        doc.duration = _leaf_value(reader, el)


def _apply_track(track, reader, el, stack):
    parent_id = stack[-1].id if stack else 0
    if parent_id == schema.VIDEO_ID:
        if el.id == 0xB0:
            track.width = _leaf_value(reader, el)
        elif el.id == 0xBA:
            track.height = _leaf_value(reader, el)
        return
    if parent_id == schema.AUDIO_ID:
        if el.id == 0xB5:
            track.sample_rate = _leaf_value(reader, el)
        elif el.id == 0x9F:
            track.channels = _leaf_value(reader, el)
        return
    if parent_id != schema.TRACK_ENTRY_ID:
        return
    if el.id == 0xD7:
        track.number = _leaf_value(reader, el)
    elif el.id == 0x73C5:
        track.uid = _leaf_value(reader, el)
    elif el.id == 0x83:
        track.type = _leaf_value(reader, el)
    elif el.id == 0x88:
        track.flag_default = bool(_leaf_value(reader, el))
    elif el.id == 0x536E:
        track.name = _leaf_value(reader, el)
    elif el.id == 0x22B59C:
        track.language = _leaf_value(reader, el)
    elif el.id == 0x86:
        track.codec_id = _leaf_value(reader, el)
    elif el.id == 0x63A2:
        track.codec_private = reader.read_data(el)
    elif el.id == 0x23E383:
        track.default_duration_ns = _leaf_value(reader, el)


def _parse_block(doc, reader, el, cluster, simple, keyframe):
    """Parse a SimpleBlock/Block header and expand lacing to samples."""
    head_len = min(el.size, 4096)
    reader.fh.seek(el.data_offset)
    head = reader.fh.read(head_len)
    if len(head) != head_len:
        raise EbmlError("truncated block header", el.offset)
    track_number, tlen = vint_from_bytes(head, 0)
    timecode = int.from_bytes(head[tlen:tlen + 2], "big", signed=True)
    flags = head[tlen + 2]
    pos = tlen + 3
    if simple:
        keyframe = bool(flags & 0x80)
    lacing = (flags >> 1) & 0x03
    sizes = _lace_sizes(head, pos, el.size, lacing, el.offset)
    header_len = sizes.pop(0)
    track = doc.track_by_number(track_number)
    if track is None:
        raise EbmlError("block references unknown track %d"
                        % track_number, el.offset)
    base_ns = (cluster.timecode + timecode) * doc.timecode_scale
    step_ns = track.default_duration_ns if len(sizes) > 1 else 0
    data = el.data_offset + header_len
    for i, size in enumerate(sizes):
        doc.samples.append(Sample(
            track_number=track_number,
            pts_ns=base_ns + i * step_ns,
            keyframe=keyframe,
            offset=data,
            size=size,
            cluster_index=cluster.index))
        data += size


def _lace_sizes(head, pos, block_size, lacing, offset):
    """Return [header_len, size0, size1, ...] for one block payload."""
    if lacing == 0:
        return [pos, block_size - pos]
    if pos >= len(head):
        raise EbmlError("truncated lace header", offset)
    count = head[pos] + 1
    pos += 1
    sizes = []
    if lacing == 1:  # Xiph lacing
        for _ in range(count - 1):
            value = 0
            while True:
                if pos >= len(head):
                    raise EbmlError("truncated Xiph lace header", offset)
                value += head[pos]
                pos += 1
                if head[pos - 1] != 255:
                    break
            sizes.append(value)
    elif lacing == 2:  # fixed-size lacing
        per = (block_size - pos) // count
        sizes = [per] * (count - 1)
    else:  # EBML lacing
        first, vlen = vint_from_bytes(head, pos)
        pos += vlen
        sizes.append(first)
        previous = first
        for _ in range(count - 2):
            raw, vlen = vint_from_bytes(head, pos)
            pos += vlen
            previous += raw - ((1 << (7 * vlen - 1)) - 1)
            sizes.append(previous)
    last = block_size - pos - sum(sizes)
    if last < 0:
        raise EbmlError("lace sizes exceed block payload", offset)
    sizes.append(last)
    return [pos] + sizes
