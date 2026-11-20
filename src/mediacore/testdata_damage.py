"""Derive damaged variants from a clean MP4 without shelling out to tools.

Every byte mutation is expressed here so a damaged fixture can be reproduced
from the exact byte range that caused it.  Three families:

* **truncation** - cut the file inside the final ``mdat`` sample.
* **corruption** - overwrite one packet's payload in place.
* **index damage** - drop a sample-to-chunk entry, or the whole table.

Deleting bytes from ``moov`` moves ``mdat``, so all absolute chunk offsets at
or after the cut are relocated; otherwise an "index gap" fixture would
accidentally corrupt every track.
"""

from __future__ import annotations

import struct
from pathlib import Path

from .errors import ParameterError

def _truncate(source: Path, target: Path) -> Path:
    data = source.read_bytes()
    mdat = data.find(b"mdat")
    if mdat < 0:
        raise ParameterError("fixture has no mdat box", path=str(source))
    # Cut 80% of the way through the media payload: enough for a valid header
    # but missing the tail of the last sample.
    start = mdat + 4
    cut = start + int((len(data) - start) * 0.8)
    target.write_bytes(data[:cut])
    return target


def _packets(path: Path) -> list[tuple[int, int, int, int]]:
    """Return ``(ordinal, pos, size, pts)`` for each video packet."""
    import av

    container = av.open(str(path))
    try:
        stream = container.streams.video[0]
        out = []
        for ordinal, packet in enumerate(container.demux(stream)):
            if packet.size == 0 and packet.pts is None:
                continue
            out.append((ordinal, int(packet.pos), packet.size, packet.pts or 0))
        return out
    finally:
        container.close()


def _corrupt_packet(source: Path, target: Path) -> Path:
    packets = _packets(source)
    if len(packets) < 10:
        raise ParameterError("fixture too short to corrupt", path=str(source))
    # Corrupt a packet near the middle so both the head and the tail remain
    # decodable: recovery is supposed to keep going.
    _, pos, size, _ = packets[len(packets) // 2]
    data = bytearray(source.read_bytes())
    payload = min(200, max(size - 4, 1))
    data[pos + 4 : pos + 4 + payload] = b"\xff" * payload
    target.write_bytes(data)
    return target


def _drop_stco_entry(source: Path, target: Path) -> Path:
    """Remove one video sample-to-chunk entry, creating an index gap.

    Only the video track's ``stco`` is shortened.  Deleting four bytes from
    inside ``moov`` moves ``mdat`` four bytes earlier, so *all* tracks' chunk
    offsets at or after the deletion are relocated — otherwise the audio
    tracks would point into the wrong bytes and the fixture would be corrupt
    in a way it was never designed to be.
    """
    data = source.read_bytes()
    found = list(_find_boxes(data, "stco"))
    if not found:
        raise ParameterError("fixture has no stco box", path=str(source))
    _, offset, size, header = found[0]  # track 0 == video
    count = struct.unpack(">I", data[offset + header + 4 : offset + header + 8])[0]
    if count < 25:
        raise ParameterError("fixture stco too small", path=str(source), count=count)
    index = count // 2
    entry = offset + header + 8 + index * 4
    removed_at = entry
    mutated = bytearray(data)
    del mutated[entry : entry + 4]
    struct.pack_into(">I", mutated, offset + header + 4, count - 1)
    _fix_ancestor_sizes(mutated, data, "stco", -4)
    _relocate_chunk_offsets(mutated, removed_at, -4)
    target.write_bytes(mutated)
    return target


def _drop_box(source: Path, target: Path, box_type: str) -> Path:
    data = source.read_bytes()
    found = list(_find_boxes(data, box_type))
    if not found:
        raise ParameterError(f"fixture has no {box_type} box", path=str(source))
    # Remove only the *first* instance (the video track's table), so the
    # fixture models one damaged track rather than a wholly broken file.
    _, offset, size, _ = found[0]
    mutated = bytearray(data)
    del mutated[offset : offset + size]
    _fix_ancestor_sizes(mutated, data, box_type, -size)
    _relocate_chunk_offsets(mutated, offset + size, -size)
    target.write_bytes(mutated)
    return target


def _relocate_chunk_offsets(mutated: bytearray, after_offset: int, delta: int) -> None:
    """Fix absolute ``stco`` offsets that point past a deleted byte range.

    ``stco`` stores absolute file offsets to each chunk.  When bytes are
    removed from anywhere before ``mdat``'s payload, *every* track's chunk
    offsets at or after the cut shift by ``delta``.  Missing any track would
    silently corrupt streams that the fixture was never meant to damage — the
    index-gap fixture must damage only the video sample table.
    """
    data = bytes(mutated)
    for found in _find_boxes(data, "stco"):
        _, offset, size, header = found
        count = struct.unpack(">I", mutated[offset + header + 4 : offset + header + 8])[0]
        base = offset + header + 8
        for i in range(count):
            at = base + i * 4
            value = struct.unpack(">I", mutated[at : at + 4])[0]
            if value >= after_offset:
                struct.pack_into(">I", mutated, at, value + delta)


def _find_boxes(data: bytes, box_type: str, *, start: int = 0, end: int | None = None):
    """Every ``box_type`` instance reachable from ``start``, depth first."""
    end = len(data) if end is None else end
    containers = {"moov", "trak", "mdia", "minf", "stbl"}
    offset = start
    while offset + 8 <= end:
        size = struct.unpack(">I", data[offset : offset + 4])[0]
        header = 8
        if size == 1:
            if offset + 16 > end:
                return
            size = struct.unpack(">Q", data[offset + 8 : offset + 16])[0]
            header = 16
        elif size == 0:
            size = end - offset
        name = data[offset + 4 : offset + 8].decode("latin1")
        if size < header or offset + size > end:
            return
        if name == box_type:
            yield name, offset, size, header
        elif name in containers:
            yield from _find_boxes(data, box_type, start=offset + header, end=offset + size)
        offset += size


def _fix_ancestor_sizes(mutated: bytearray, original: bytes, box_type: str, delta: int) -> None:
    """Patch the size fields of every container box around ``box_type``."""
    chain: list[tuple[int, int]] = []

    def walk(start: int, end: int) -> bool:
        offset = start
        while offset + 8 <= end:
            size = struct.unpack(">I", original[offset : offset + 4])[0]
            header = 8
            if size == 1:
                size = struct.unpack(">Q", original[offset + 8 : offset + 16])[0]
                header = 16
            elif size == 0:
                size = end - offset
            name = original[offset + 4 : offset + 8].decode("latin1")
            if name == box_type:
                return True
            if name in ("moov", "trak", "mdia", "minf", "stbl", "udta"):
                chain.append((offset, size))
                if walk(offset + header, offset + size):
                    return True
                chain.pop()
            if size < header:
                break
            offset += size
        return False

    walk(0, len(original))
    for offset, size in chain:
        struct.pack_into(">I", mutated, offset, size + delta)

