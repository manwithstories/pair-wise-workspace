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


def track_table(init: bytes) -> tuple[dict[int, int], dict[int, int], dict[int, int]]:
    """Read ``trak`` order, ``tkhd`` ids and ``mdhd`` timescales from ``moov``.

    Returns ``(track_id -> track order, track_id -> timescale,
    track order -> track_id)``.
    """
    moov = find_box(init, ("moov",))
    if moov is None:
        raise StructuralError("initialisation segment has no moov box")
    offset, size = moov
    order: dict[int, int] = {}
    timescales: dict[int, int] = {}
    by_order: dict[int, int] = {}
    track_index = 0
    for box_type, box_offset, box_size in iter_children(init, offset, size):
        if box_type != "trak":
            continue
        track_id = None
        timescale = None
        for child_type, child_offset, child_size in iter_children(init, box_offset, box_size):
            if child_type == "tkhd":
                version = init[child_offset + 8]
                track_id = _versioned_u32(init, child_offset, version, 8, 16)
            elif child_type == "mdia":
                for media_type, media_offset, media_size in iter_children(init, child_offset, child_size):
                    if media_type == "mdhd":
                        version = init[media_offset + 8]
                        timescale = _versioned_u32(init, media_offset, version, 8, 16)
        if track_id is not None:
            order[track_id] = track_index
            by_order[track_index] = track_id
            if timescale:
                timescales[track_id] = timescale
        track_index += 1
    return order, timescales, by_order


# ---------------------------------------------------------------------------
# Encoding
# ---------------------------------------------------------------------------
@dataclass
class EncodedFragment:
    data: bytes
    samples: list[SampleRef]
    first_times: dict[int, int]
    timescales: dict[int, int]
    timescales_by_stream: dict[int, int]

    def body(self) -> bytes:
        """Everything except ``ftyp``/``moov``/``mfra`` (used after chunk 0)."""
        parts = []
        for box_type, offset, size in iter_boxes(self.data):
            if box_type in ("moof", "mdat"):
                parts.append(self.data[offset:offset + size])
        return b"".join(parts)

    def init_segment(self) -> bytes:
        parts = []
        for box_type, offset, size in iter_boxes(self.data):
            if box_type in ("ftyp", "moov"):
                parts.append(self.data[offset:offset + size])
        return b"".join(parts)


class ChunkEncoder:
    """Encode one chunk of packets into a self-contained MP4 fragment.

    ``templates`` are the source streams, so codec parameters and extradata
    pass through unchanged; nothing is transcoded.  ``track_ids`` is learned
    from the first fragment and reused, guaranteeing stable ``track_id``
    values across chunks.
    """

    def __init__(self, templates: Iterable[av.stream.Stream]):
        self.templates = list(templates)
        self.track_ids: dict[int, int] = {}
        self.stream_by_track: dict[int, int] = {}
        self.timescales_by_stream: dict[int, int] = {}

    def encode(self, packets: Iterable[av.Packet],
               absolute_start: dict[int, int] | None = None) -> EncodedFragment:
        """Mux one chunk.

        ``packets`` must carry chunk-relative timestamps in their source time
        base.  ``absolute_start`` maps stream index to the repaired chunk
        start in the output media timescale; every ``tfdt`` of that track is
        rebased so samples land on the global timeline.  Omit it to keep
        chunk-local times (used for probing).
        """
        buffer = io.BytesIO()
        try:
            container = av.open(buffer, "w", format="mp4", options=dict(MUX_OPTIONS))
        except Exception as exc:  # pragma: no cover - environmental
            raise StructuralError(f"cannot open fragment muxer: {exc}") from exc
        for template in self.templates:
            container.add_stream(template=template)
        try:
            for packet in packets:
                if packet.stream is None:
                    raise StructuralError("packet is not associated with a stream")
                packet.stream = container.streams[packet.stream.index]
                container.mux(packet)
            container.close()
        except Exception as exc:
            try:
                container.close()
            except Exception:
                pass
            raise IndexWriteError(f"could not encode chunk: {exc}") from exc
        data = buffer.getvalue()
        if not data:
            raise IndexWriteError("fragment muxer produced no output")

        init = self._init_segment(data)
        order, timescales, by_order = track_table(init)
        self.track_ids = {stream: track for track, stream in order.items()}
        self.stream_by_track = {track: stream for track, stream in order.items()}
        self.timescales_by_stream = {
            stream: timescales[track] for track, stream in order.items() if track in timescales
        }

        if absolute_start:
            data = rebase_tfdt(data, absolute_start, self.track_ids)

        samples: list[SampleRef] = []
        first_times: dict[int, int] = {}
        first_moof = None
        for box_type, offset, size in iter_boxes(data):
            if box_type != "moof":
                continue
            if first_moof is None:
                first_moof = offset
            moof_samples, headers = parse_moof(
                data, offset, size, self.stream_by_track, first_moof
            )
            samples.extend(moof_samples)
            for header in headers:
                if header.decode_time is not None:
                    first_times.setdefault(header.track_id, header.decode_time)
        return EncodedFragment(
            data=data,
            samples=samples,
            first_times=first_times,
            timescales=timescales,
            timescales_by_stream=dict(self.timescales_by_stream),
        )

    @staticmethod
    def _init_segment(data: bytes) -> bytes:
        parts = []
        for box_type, offset, size in iter_boxes(data):
            if box_type in ("ftyp", "moov"):
                parts.append(data[offset:offset + size])
        return b"".join(parts)


def rebase_tfdt(data: bytes, absolute_start: dict[int, int],
                stream_to_track: dict[int, int]) -> bytes:
    """Shift every ``tfdt`` of a fragment onto the global timeline.

    ``absolute_start`` is the repaired chunk start per stream in output media
    ticks.  The first ``traf`` of each track supplies the chunk-local start
    (normally zero); subsequent ``traf`` boxes of the same track are shifted
    by the same delta so the fragment keeps its internal spacing.
    """
    track_start = {stream_to_track[stream]: ticks
                   for stream, ticks in absolute_start.items() if stream in stream_to_track}
    if not track_start:
        return data
    buffer = bytearray(data)
    base_by_track: dict[int, int] = {}
    for box_type, offset, size in iter_boxes(data):
        if box_type != "moof":
            continue
        for traf_type, traf_offset, traf_size in iter_children(data, offset, size):
            if traf_type != "traf":
                continue
            track_id = None
            tfdt_offset = None
            for child_type, child_offset, _ in iter_children(data, traf_offset, traf_size):
                if child_type == "tfhd":
                    track_id = struct.unpack(">I", data[child_offset + 12:child_offset + 16])[0]
                elif child_type == "tfdt":
                    tfdt_offset = child_offset
            if track_id is None or tfdt_offset is None or track_id not in track_start:
                continue
            version = data[tfdt_offset + 8]
            base = tfdt_offset + 12
            if version == 1:
                current = struct.unpack(">Q", data[base:base + 8])[0]
            else:
                current = struct.unpack(">I", data[base:base + 4])[0]
            if track_id not in base_by_track:
                base_by_track[track_id] = current
            delta = track_start[track_id] - base_by_track[track_id]
            new_value = current + delta
            if version == 1:
                struct.pack_into(">Q", buffer, base, new_value & 0xFFFFFFFFFFFFFFFF)
            else:
                struct.pack_into(">I", buffer, base, new_value & 0xFFFFFFFF)
    return bytes(buffer)


def read_init_segment(path) -> bytes:
    """Return the leading ``ftyp``/``moov`` boxes of a repaired file."""
    from pathlib import Path
    path = Path(path)
    with open(path, "rb") as handle:
        head = handle.read(4 << 20)
        parts = []
        for box_type, offset, size in iter_boxes(head):
            if box_type in ("ftyp", "moov"):
                parts.append(head[offset:offset + size])
            else:
                break
        return b"".join(parts)


def patch_durations(init: bytes, durations: dict[int, int], movie_duration: int,
                    movie_timescale: int | None = None) -> bytes:
    """Rewrite ``mvhd``/``tkhd``/``mdhd`` durations in the init segment.

    The muxer writes durations from the packets it saw per chunk, but when
    the timeline is rebased (clock jumps repaired) or assembled from many
    fragments, the totals must describe the final file.  If the container
    advertises a duration shorter than its last sample, libavformat refuses
    to seek - which would break the random-access guarantee.
    """
    buffer = bytearray(init)
    moov = find_box(init, ("moov",))
    if moov is None:
        return init
    offset, size = moov
    for box_type, box_offset, box_size in iter_children(init, offset, size):
        if box_type == "mvhd":
            version = init[box_offset + 8]
            base = box_offset + 12
            timescale_field = base + (16 if version == 1 else 8)
            if movie_timescale is not None:
                struct.pack_into(">I", buffer, timescale_field, movie_timescale)
            actual_timescale = struct.unpack(
                ">I", init[timescale_field:timescale_field + 4])[0] or 1000
            ticks = max(0, int(round(movie_duration * actual_timescale)))
            if version == 1:
                struct.pack_into(">Q", buffer, base + 20, ticks)
            else:
                struct.pack_into(">I", buffer, base + 12, min(ticks, 0xFFFFFFFF))
        elif box_type == "trak":
            track_id = None
            mdhd_offset = None
            tkhd_offset = None
            mdhd_version = 0
            for child_type, child_offset, child_size in iter_children(init, box_offset, box_size):
                if child_type == "tkhd":
                    tkhd_offset = child_offset
                    version = init[child_offset + 8]
                    base = child_offset + 12
                    track_id = struct.unpack(
                        ">I", init[base + (16 if version == 1 else 8):base + (20 if version == 1 else 12)]
                    )[0]
                elif child_type == "mdia":
                    for media_type, media_offset, media_size in iter_children(init, child_offset, child_size):
                        if media_type == "mdhd":
                            mdhd_offset = media_offset
                            mdhd_version = init[media_offset + 8]
            if track_id is None or track_id not in durations:
                continue
            seconds = durations[track_id]
            if mdhd_offset is not None:
                timescale_field = mdhd_offset + 12 + (16 if mdhd_version == 1 else 8)
                timescale = struct.unpack(">I", init[timescale_field:timescale_field + 4])[0] or 1
                ticks = int(round(seconds * timescale))
                duration_field = mdhd_offset + 12 + (24 if mdhd_version == 1 else 16)
                if mdhd_version == 1:
                    struct.pack_into(">Q", buffer, duration_field, ticks)
                else:
                    struct.pack_into(">I", buffer, duration_field, min(ticks, 0xFFFFFFFF))
            if tkhd_offset is not None:
                version = init[tkhd_offset + 8]
                base = tkhd_offset + 12
                # tkhd uses the movie timescale taken from mvhd.
                movie_ts = movie_timescale or 1000
                movie_ticks = int(round(seconds * movie_ts))
                if version == 1:
                    struct.pack_into(">Q", buffer, base + 24, movie_ticks)
                else:
                    struct.pack_into(">I", buffer, base + 16, min(movie_ticks, 0xFFFFFFFF))
    return bytes(buffer)


def discover_timescales(encoder: ChunkEncoder, sample_packets: Iterable[av.Packet]) -> dict[int, int]:
    """Learn the muxer's output timescale per stream from a probe fragment."""
    fragment = encoder.encode(list(sample_packets))
    return dict(fragment.timescales_by_stream)
