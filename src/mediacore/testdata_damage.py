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
    """Remove one video *sample* from the index, creating a PTS-visible gap.

    A real "index gap" means the container's sample tables disagree with the
    media: one sample is missing from the sample-to-chunk table (``stco``) and
    from the sample-size table (``stsz``), but its *duration* remains in the
    time-to-sample table (``stts``).  The effect is exactly what a damaged
    transport stream looks like: the PTS interval across the hole is two
    packet durations, so the gap is locatable to the frame.

    Dropping only the ``stco`` entry (without ``stsz``) would renumber the
    surviving samples and hide the gap from timestamps — not what we want to
    test.  ``stts`` keeps its total duration, so the shortfall is visible both
    as a PTS jump and as ``declared > demuxed``.
    """
    data = source.read_bytes()
    stco = list(_find_boxes(data, "stco"))
    stsz = list(_find_boxes(data, "stsz"))
    if not stco or not stsz:
        raise ParameterError("fixture lacks stco/stsz", path=str(source))
    _, stco_off, _, stco_hdr = stco[0]  # track 0 == video
    count = struct.unpack(">I", data[stco_off + stco_hdr + 4 : stco_off + stco_hdr + 8])[0]
    if count < 25:
        raise ParameterError("fixture stco too small", path=str(source), count=count)
    index = count - 1  # drop the *last* sample: a tail index gap

    # Remove the stsz sample-size entry.  stsz layout: version/flags (4),
    # sample_size (4), sample_count (4), then one 4-byte size per sample.
    _, stsz_off, _, stsz_hdr = stsz[0]
    stsz_count_at = stsz_off + stsz_hdr + 8
    stsz_entries = stsz_count_at + 4
    size_entry = stsz_entries + index * 4
    # Remove the stco chunk-offset entry.
    stco_entry = stco_off + stco_hdr + 8 + index * 4

    # Patch the two count fields *before* deleting, while the original offsets
    # are still valid (deleting the stsz entry shifts the later stco box).
    mutated = bytearray(data)
    struct.pack_into(">I", mutated, stsz_count_at, count - 1)
    struct.pack_into(">I", mutated, stco_off + stco_hdr + 4, count - 1)
    # Each box also shrinks by one 4-byte entry; patch its own size field.
    struct.pack_into(">I", mutated, stsz_off, struct.unpack(">I", data[stsz_off:stsz_off+4])[0] - 4)
    struct.pack_into(">I", mutated, stco_off, struct.unpack(">I", data[stco_off:stco_off+4])[0] - 4)
    # Delete the later range first so the earlier offset stays valid.
    cut_a, cut_b = sorted((size_entry, stco_entry))
    del mutated[cut_b : cut_b + 4]
    del mutated[cut_a : cut_a + 4]
    # Every ancestor of the stbl (moov/trak/mdia/minf/stbl) shrinks by 8.
    _fix_ancestor_sizes(mutated, data, "stsz", -8)
    # ``mdat`` moved 8 bytes earlier, so every remaining chunk offset that
    # pointed into it must shift by -8 (the deleted stco entry is already gone
    # from the relocated table).
    _relocate_chunk_offsets(mutated, 0, -8)
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

