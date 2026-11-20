"""Minimal fragmented-MP4 writer used to assemble repaired output.

Why fragments instead of a plain MOV
------------------------------------
A repaired file must satisfy three requirements at once:

1. It must decode in ordinary players (H.264 + AAC in MP4/MOV).
2. It must support *random access* to any repaired keyframe - this is what
   ``verify`` exercises and what the index promises.
3. A resumed run must produce byte-identical output to an uninterrupted run.

Plain (non-fragmented) MOV cannot satisfy (2) when the file is assembled
incrementally: the seek table (``stss``/``stco``) lives only in the trailing
``moov``, so a partially written file cannot be opened at all.  Fragmented
MP4 keeps one initialisation header (``ftyp`` + ``moov`` with ``mvex``) and
then one ``moof``+``mdat`` pair per chunk, so every committed chunk is usable
by the index immediately and the file can be appended to - or rebuilt from a
later chunk - without rewriting earlier bytes.

How it works
------------
:class:`ChunkEncoder` accepts one chunk of ``av.Packet`` objects whose
``dts``/``pts`` are expressed relative to the chunk (each stream starts at
zero) in the packet's original time base.  Chunk-relative input keeps the
muxer's sample bookkeeping simple and makes ``trun`` composition offsets
exact.  After muxing, every ``tfdt`` of a track is rebased by

    new = old + (absolute_start_ticks - first_tfdt_ticks)

where ``absolute_start_ticks`` is the repaired start of the chunk in the
output media timescale.  Because all fragments of a chunk share one ``moof``
origin and ``omit_tfhd_offset`` makes sample offsets moof-relative, the
fragments stay valid wherever they land in the final file.

For every sample the encoder reports the offset of its payload relative to
the fragment's first ``moof``.  Adding the fragment's absolute body offset
yields the byte offsets stored in the random-access index.
"""

from __future__ import annotations

import io
import struct
from dataclasses import dataclass
from typing import Iterable, Sequence

import av

from .errors import IndexWriteError, StructuralError

#: ``empty_moov`` = init segment without samples; ``frag_keyframe`` = a new
#: ``moof`` at each keyframe so random access lands on a fragment start;
#: ``omit_tfhd_offset`` = sample offsets relative to ``moof``.
FRAGMENT_FLAGS = "+frag_keyframe+empty_moov+default_base_moof+omit_tfhd_offset"

#: Deterministic muxer options (``bitexact`` drops encoder/version strings).
MUX_OPTIONS = {"movflags": FRAGMENT_FLAGS, "fflags": "+bitexact"}


# ---------------------------------------------------------------------------
# Box helpers
# ---------------------------------------------------------------------------
def iter_boxes(data: bytes, start: int = 0, end: int | None = None):
    """Yield ``(type, offset, size)`` for boxes between ``start`` and ``end``."""
    end = len(data) if end is None else end
    index = start
    while index + 8 <= end:
        size = struct.unpack(">I", data[index:index + 4])[0]
        box_type = data[index + 4:index + 8].decode("latin-1")
        header = 8
        if size == 1:
            if index + 16 > end:
                raise StructuralError("truncated extended box header")
            size = struct.unpack(">Q", data[index + 8:index + 16])[0]
            header = 16
        elif size == 0:
            size = end - index
        if size < header or index + size > end:
            raise StructuralError(f"box {box_type!r} at {index} overruns the buffer")
        yield box_type, index, size
        index += size


def iter_children(data: bytes, offset: int, size: int):
    """Yield the child boxes of the box starting at ``offset``."""
    parent_size = struct.unpack(">I", data[offset:offset + 4])[0]
    header = 16 if parent_size == 1 else 8
    yield from iter_boxes(data, offset + header, offset + size)


def find_box(data: bytes, path: Sequence[str]) -> tuple[int, int] | None:
    """Locate a nested box by type path, e.g. ``("moov", "trak", "tkhd")``."""
    offset, size = 0, len(data)
    start = 0
    for depth, wanted in enumerate(path):
        found = None
        for box_type, box_offset, box_size in iter_boxes(data, start, offset + size if depth else None):
            if box_type == wanted:
                found = (box_offset, box_size)
                break
        if found is None:
            return None
        offset, size = found
        parent_size = struct.unpack(">I", data[offset:offset + 4])[0]
        start = offset + (16 if parent_size == 1 else 8)
    return offset, size


def make_box(box_type: bytes, payload: bytes) -> bytes:
    return struct.pack(">I", 8 + len(payload)) + box_type + payload


def _versioned_u32(data: bytes, offset: int, version: int, short_skip: int, long_skip: int) -> int:
    base = offset + 12
    return struct.unpack(
        ">I", data[base + (long_skip if version == 1 else short_skip):
                  base + (long_skip + 4 if version == 1 else short_skip + 4)]
    )[0]


# ---------------------------------------------------------------------------
# Fragment parsing
# ---------------------------------------------------------------------------
@dataclass
class SampleRef:
    """One sample inside a fragment.

    ``frag_offset`` is relative to the *first* ``moof`` of the fragment.
    ``body()`` skips ``ftyp``/``moov``/``mfra`` but keeps every ``moof`` and
    ``mdat`` box in order, so this relative offset survives splicing
    unchanged: ``absolute = body_file_offset + frag_offset``.
    """

    track_id: int
    stream_index: int
    frag_offset: int
    size: int
    duration: int | None
    cto: int | None
    flags: int | None


@dataclass
class TrafHeader:
    track_id: int
    decode_time: int | None


def parse_moof(data: bytes, moof_offset: int, moof_size: int,
               stream_by_track: dict[int, int],
               origin_offset: int) -> tuple[list[SampleRef], list[TrafHeader]]:
    """Return samples and per-traf headers of one ``moof``.

    ``stream_by_track`` maps the container ``track_id`` to the source stream
    index.  ``origin_offset`` is the byte offset of the fragment's first
    ``moof``; sample offsets are expressed relative to it so they remain
    valid after the fragment has been spliced into the output file.
    """
    samples: list[SampleRef] = []
    headers: list[TrafHeader] = []
    for box_type, offset, size in iter_children(data, moof_offset, moof_size):
        if box_type != "traf":
            continue
        track_id = None
        default_size = None
        default_duration = None
        decode_time = None
        for child_type, child_offset, child_size in iter_children(data, offset, size):
            if child_type == "tfhd":
                flags = struct.unpack(">I", data[child_offset + 8:child_offset + 12])[0] & 0xFFFFFF
                track_id = struct.unpack(">I", data[child_offset + 12:child_offset + 16])[0]
                cursor = child_offset + 16
                if flags & 0x01:
                    cursor += 8  # base_data_offset
                if flags & 0x02:
                    cursor += 4  # sample_description_index
                if flags & 0x08:
                    default_duration = struct.unpack(">I", data[cursor:cursor + 4])[0]
                    cursor += 4
                if flags & 0x10:
                    default_size = struct.unpack(">I", data[cursor:cursor + 4])[0]
                    cursor += 4
                if flags & 0x20:
                    cursor += 4  # default_sample_flags
            elif child_type == "tfdt":
                version = data[child_offset + 8]
                base = child_offset + 12
                if version == 1:
                    decode_time = struct.unpack(">Q", data[base:base + 8])[0]
                else:
                    decode_time = struct.unpack(">I", data[base:base + 4])[0]
        if track_id is None:
            continue
        headers.append(TrafHeader(track_id=track_id, decode_time=decode_time))
        if track_id not in stream_by_track:
            continue
        stream_index = stream_by_track[track_id]
        for child_type, child_offset, child_size in iter_children(data, offset, size):
            if child_type != "trun":
                continue
            flags = struct.unpack(">I", data[child_offset + 8:child_offset + 12])[0] & 0xFFFFFF
            count = struct.unpack(">I", data[child_offset + 12:child_offset + 16])[0]
            cursor = child_offset + 16
            data_offset = 0
            if flags & 0x000001:
                data_offset = struct.unpack(">i", data[cursor:cursor + 4])[0]
                cursor += 4
            if flags & 0x000004:
                cursor += 4
            position = moof_offset + data_offset
            for _ in range(count):
                duration = default_duration
                sample_size = default_size
                sample_flags = None
                cto = None
                if flags & 0x000100:
                    duration = struct.unpack(">I", data[cursor:cursor + 4])[0]
                    cursor += 4
                elif default_duration is not None:
                    duration = default_duration
                if flags & 0x000200:
                    sample_size = struct.unpack(">I", data[cursor:cursor + 4])[0]
                    cursor += 4
                elif default_size is not None:
                    sample_size = default_size
                if flags & 0x000400:
                    sample_flags = struct.unpack(">I", data[cursor:cursor + 4])[0]
                    cursor += 4
                if flags & 0x000800:
                    cto = struct.unpack(">i", data[cursor:cursor + 4])[0]
                    cursor += 4
                if sample_size is None:
                    raise StructuralError("fragment sample carries no size")
                samples.append(SampleRef(
                    track_id=track_id,
                    stream_index=stream_index,
                    frag_offset=position - origin_offset,
                    size=sample_size,
                    duration=duration,
                    cto=cto,
                    flags=sample_flags,
                ))
                position += sample_size
    return samples, headers


